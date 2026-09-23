package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry kinds. Migration-only now: MeetingStore (meetingstore.go) is where a
// meeting lives once recorded, and nothing writes Kind or reads these
// constants except MigrateMeetings, which needs them to pick meetings out of
// day files written before that store existed. Keep them around as long as
// any such file can still be on disk -- deleting them would make those files
// unreadable.
const (
	KindDictation = "dictation"
	KindMeeting   = "meeting"
)

type Entry struct {
	Timestamp        time.Time `json:"timestamp"`
	DurationSeconds  float64   `json:"duration_seconds"`
	Text             string    `json:"text"`
	RecordingSeconds float64   `json:"recording_seconds"`
	// Kind marked a meeting recording, back when meetings lived in this same
	// file. Migration-only now -- see the comment on the constants above.
	Kind string `json:"kind,omitempty"`
	// AudioPath now also holds where a dictation's own recording landed (see
	// saveDictationAudio in dictate.go); a dictation never uses
	// SystemAudioPath, which MigrateMeetings still reads off old files from
	// back when a meeting's audio lived in this same store.
	AudioPath       string `json:"audio_path,omitempty"`
	SystemAudioPath string `json:"system_audio_path,omitempty"`
	// AutoStarted marks a note always-on listening recorded on its own --
	// one voice, no second party, nobody pressed a key. It is what retention
	// reads (an auto note with no transcript is a guess that did not pay
	// off) and what the list shows, so a line nobody dictated is never
	// mistaken for one that was.
	AutoStarted bool `json:"auto_started,omitempty"`
}

// Store is the dictations half of history. Its public surface is unchanged
// from when it was one JSON file per day; what is behind it is now rows in
// the same database the meetings live in (see db.go). The day files are still
// read -- by the migration, and by nothing else.
//
// The mutex this used to hold is gone with the read-modify-write it guarded:
// an append is one INSERT, and the database is opened with SetMaxOpenConns(1),
// so two transcripts finishing at once serialize on the connection instead of
// on a lock here.
type Store struct {
	dir string

	// Opened on first use, for the same reason MeetingStore's is: the app
	// builds its stores before it knows whether anything is writable, and a
	// first run has no directory yet.
	once    sync.Once
	db      *DB
	openErr error
}

func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "Documents", "Voxlog", "Transcripts")
}

func NewStore(dir string) *Store {
	if dir == "" {
		dir = DefaultDir()
	}
	return &Store{dir: dir}
}

// NewStoreDB is the app's constructor: the database lives beside the settings
// while dir stays where the user pointed their transcripts, which may well be
// a synced folder. Same split, and same reason, as NewMeetingStoreDB.
func NewStoreDB(dir string, db *DB) *Store {
	s := NewStore(dir)
	s.once.Do(func() { s.db = db })
	return s
}

func (s *Store) open() (*DB, error) {
	s.once.Do(func() {
		s.db, s.openErr = OpenDB(filepath.Join(s.dir, "voxlog.db"))
	})
	if s.openErr != nil {
		return nil, s.openErr
	}
	return s.db, nil
}

// Dir is where the day files live. Meeting recordings are kept alongside
// them, so the audio and the transcript it produced travel together and one
// retention policy covers both.
func (s *Store) Dir() string { return s.dir }

// readDay and writeDay are the day-file layer, kept for the migration and
// for nothing else: MigrateDictations reads with one, and MigrateMeetings
// still rewrites a day file with the other.
func (s *Store) readDay(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *Store) writeDay(path string, entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Append writes one dictation, replacing any record already under that
// instant. Replacing rather than failing for the same reason MeetingStore
// does it: a re-run of a migration re-states what it already knows.
func (s *Store) Append(e Entry) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(`
		INSERT INTO dictations
			(ts_ns, duration_secs, recording_secs, text, audio_path, system_audio_path, auto_started)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_ns) DO UPDATE SET
			duration_secs     = excluded.duration_secs,
			recording_secs    = excluded.recording_secs,
			text              = excluded.text,
			audio_path        = excluded.audio_path,
			system_audio_path = excluded.system_audio_path,
			auto_started      = excluded.auto_started`,
		e.Timestamp.UnixNano(), e.DurationSeconds, e.RecordingSeconds, e.Text,
		e.AudioPath, e.SystemAudioPath, e.AutoStarted)
	if err != nil {
		return fmt.Errorf("history: appending dictation: %w", err)
	}
	return nil
}

// Update applies mutate to the entry with the given timestamp and writes the
// day file back. This is how a transcript reaches an entry that was written
// before it existed: a meeting lands in history the moment it stops, and the
// text arrives minutes later when its decode finishes.
//
// Timestamps identify entries here because they are what the History window
// already keys on, and two takes cannot start in the same nanosecond.
// The read and the write are one immediate transaction: a transcript landing
// while retention clears an audio path must not splice their fields together.
func (s *Store) Update(timestamp time.Time, mutate func(*Entry)) error {
	db, err := s.open()
	if err != nil {
		return err
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: updating dictation: %w", err)
	}
	defer tx.Rollback()

	e, err := scanEntry(tx.QueryRow(selectDictationSQL+" WHERE ts_ns = ?", timestamp.UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("history: no entry at %s", timestamp.Format(time.RFC3339Nano))
	}
	if err != nil {
		return fmt.Errorf("history: reading dictation %s: %w", timestamp.Format(time.RFC3339Nano), err)
	}

	mutate(&e)

	_, err = tx.Exec(`
		UPDATE dictations SET
			duration_secs     = ?,
			recording_secs    = ?,
			text              = ?,
			audio_path        = ?,
			system_audio_path = ?,
			auto_started      = ?
		WHERE ts_ns = ?`,
		e.DurationSeconds, e.RecordingSeconds, e.Text, e.AudioPath,
		e.SystemAudioPath, e.AutoStarted, timestamp.UnixNano())
	if err != nil {
		return fmt.Errorf("history: writing dictation %s: %w", timestamp.Format(time.RFC3339Nano), err)
	}
	return tx.Commit()
}

const selectDictationSQL = `
	SELECT ts_ns, duration_secs, recording_secs, text, audio_path,
	       system_audio_path, auto_started
	FROM dictations`

func scanEntry(row rowScanner) (Entry, error) {
	var (
		e    Entry
		tsNS int64
	)
	err := row.Scan(&tsNS, &e.DurationSeconds, &e.RecordingSeconds, &e.Text,
		&e.AudioPath, &e.SystemAudioPath, &e.AutoStarted)
	if err != nil {
		return Entry{}, err
	}
	e.Timestamp = time.Unix(0, tsNS)
	return e, nil
}

// EntriesForDay returns one local calendar day's dictations, oldest first.
// The day is a range over ts_ns now rather than a filename, which is also why
// retention no longer has to delete in whole-day units.
func (s *Store) EntriesForDay(day time.Time) ([]Entry, error) {
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	until := from.AddDate(0, 0, 1)
	return s.query(selectDictationSQL+" WHERE ts_ns >= ? AND ts_ns < ? ORDER BY ts_ns",
		from.UnixNano(), until.UnixNano())
}

// AllEntries returns every dictation, oldest first -- the order the History
// window and the MCP tools have always been handed them in.
func (s *Store) AllEntries() ([]Entry, error) {
	return s.query(selectDictationSQL + " ORDER BY ts_ns")
}

func (s *Store) query(sqlText string, args ...any) ([]Entry, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("history: listing dictations: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("history: listing dictations: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
