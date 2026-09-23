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

// A meeting is one row, keyed by when it started. Meetings arrive a few a day
// rather than dozens an hour, and each carries fields a dictation never will
// -- two audio tracks, a length measured in hours, and now a list of turns
// with a speaker each -- so grouping them into the day files alongside
// dictations is what forced the Kind discriminator this store exists to
// retire.
//
// The JSON tags are still here because the migration reads the old
// one-file-per-meeting records with this same type.
type Meeting struct {
	Start            time.Time `json:"start"`
	RecordingSeconds float64   `json:"recording_seconds"`
	Text             string    `json:"text,omitempty"`
	DurationSeconds  float64   `json:"duration_seconds,omitempty"`
	AudioPath        string    `json:"audio_path,omitempty"`
	SystemAudioPath  string    `json:"system_audio_path,omitempty"`
	// Summary is Task Hub's short LLM summary of Text, filled in once the
	// transcript is available. Empty until then, or if Task Hub is off.
	Summary string `json:"summary,omitempty"`
	// Entity is the project this meeting belongs to, set by hand from the
	// window. Empty means unfiled; the list falls back to the project of any
	// task Task Hub found in the meeting.
	Entity string `json:"entity,omitempty"`
	// TurnsVersion is the turn-extraction version behind this meeting's rows
	// in the turns table. 0 means it has none -- either it predates them or
	// its decode never got that far.
	TurnsVersion int `json:"-"`
	// AutoStarted marks a recording always-on listening began by itself,
	// rather than one the user started with the key or the menu. Retention
	// is the reason it exists: an auto-started recording with no transcript
	// is a guess that did not pay off, and it is swept within a day.
	AutoStarted bool `json:"auto_started,omitempty"`
}

// MeetingStore is the meetings half of history. Its public surface is
// unchanged from when it was one JSON file per meeting; only what is behind
// it moved into SQLite (see db.go for why).
type MeetingStore struct {
	dir string

	// The database is opened on first use rather than in the constructor,
	// which cannot return an error: the app builds its stores long before it
	// knows whether the user's transcripts directory is writable, and a first
	// run has no directory at all yet. Whatever the open cost was, every
	// method reports it.
	once    sync.Once
	db      *DB
	openErr error
}

func DefaultMeetingsDir() string {
	return filepath.Join(DefaultDir(), "Meetings")
}

func NewMeetingStore(dir string) *MeetingStore {
	if dir == "" {
		dir = DefaultMeetingsDir()
	}
	return &MeetingStore{dir: dir}
}

// NewMeetingStoreDB is the app's constructor: the recordings and the frozen
// JSON live in the transcripts directory, which the user can point anywhere
// (including at a cloud-synced folder), while the database lives beside the
// settings. SQLite over a syncing filesystem is a well-known way to corrupt
// one, so the two paths are deliberately allowed to differ.
func NewMeetingStoreDB(dir string, db *DB) *MeetingStore {
	s := NewMeetingStore(dir)
	s.once.Do(func() { s.db = db })
	return s
}

// Dir is where meeting audio lives -- unchanged, and still not where the
// database is.
func (s *MeetingStore) Dir() string { return s.dir }

func (s *MeetingStore) open() (*DB, error) {
	s.once.Do(func() {
		s.db, s.openErr = OpenDB(filepath.Join(s.dir, "voxlog.db"))
	})
	if s.openErr != nil {
		return nil, s.openErr
	}
	return s.db, nil
}

// Append writes the meeting, replacing any record already under that start
// instant. A re-run of the same append is how a crashed migration or an
// orphan sweep re-states what it knows, so it must not fail on conflict.
func (s *MeetingStore) Append(m Meeting) error {
	db, err := s.open()
	if err != nil {
		return err
	}

	_, err = db.sql.Exec(`
		INSERT INTO meetings
			(start_ns, recording_secs, decode_secs, text, summary, audio_path, system_audio_path, entity, auto_started)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(start_ns) DO UPDATE SET
			recording_secs    = excluded.recording_secs,
			decode_secs       = excluded.decode_secs,
			text              = excluded.text,
			summary           = excluded.summary,
			audio_path        = excluded.audio_path,
			system_audio_path = excluded.system_audio_path,
			entity            = excluded.entity,
			auto_started      = excluded.auto_started`,
		m.Start.UnixNano(), m.RecordingSeconds, m.DurationSeconds, m.Text,
		m.Summary, m.AudioPath, m.SystemAudioPath, m.Entity, m.AutoStarted)
	if err != nil {
		return fmt.Errorf("history: appending meeting: %w", err)
	}
	return nil
}

// Update applies mutate to the meeting starting at start and writes it back.
// This is how a transcript reaches a meeting that was recorded before it
// existed: the meeting lands in history the moment the call ends, and the
// text arrives minutes later when its decode finishes.
//
// The read and the write are one immediate transaction, which is what used to
// need a mutex: two writers touching the SAME meeting -- a transcript landing
// while a retention sweep clears the audio path -- must not splice their
// fields together.
func (s *MeetingStore) Update(start time.Time, mutate func(*Meeting)) error {
	db, err := s.open()
	if err != nil {
		return err
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: updating meeting: %w", err)
	}
	defer tx.Rollback()

	m, err := scanMeeting(tx.QueryRow(selectMeetingSQL+" WHERE start_ns = ?", start.UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("history: no meeting at %s", start.Format(time.RFC3339Nano))
	}
	if err != nil {
		return fmt.Errorf("history: reading meeting %s: %w", start.Format(time.RFC3339Nano), err)
	}

	mutate(&m)

	_, err = tx.Exec(`
		UPDATE meetings SET
			recording_secs    = ?,
			decode_secs       = ?,
			text              = ?,
			summary           = ?,
			audio_path        = ?,
			system_audio_path = ?,
			entity            = ?,
			auto_started      = ?
		WHERE start_ns = ?`,
		m.RecordingSeconds, m.DurationSeconds, m.Text, m.Summary,
		m.AudioPath, m.SystemAudioPath, m.Entity, m.AutoStarted, start.UnixNano())
	if err != nil {
		return fmt.Errorf("history: writing meeting %s: %w", start.Format(time.RFC3339Nano), err)
	}
	return tx.Commit()
}

// SetEntity files a meeting under a project by hand. Separate from Update
// because the window calls it on its own, and because it must not disturb a
// transcript that may be landing at the same moment.
func (s *MeetingStore) SetEntity(start time.Time, entity string) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	res, err := db.sql.Exec("UPDATE meetings SET entity = ? WHERE start_ns = ?", entity, start.UnixNano())
	if err != nil {
		return fmt.Errorf("history: filing meeting: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("history: no meeting at %s", start.Format(time.RFC3339Nano))
	}
	return nil
}

const selectMeetingSQL = `
	SELECT start_ns, recording_secs, decode_secs, text, summary,
	       audio_path, system_audio_path, entity, turns_version, auto_started
	FROM meetings`

type rowScanner interface{ Scan(dest ...any) error }

func scanMeeting(row rowScanner) (Meeting, error) {
	var (
		m       Meeting
		startNS int64
	)
	err := row.Scan(&startNS, &m.RecordingSeconds, &m.DurationSeconds, &m.Text,
		&m.Summary, &m.AudioPath, &m.SystemAudioPath, &m.Entity, &m.TurnsVersion, &m.AutoStarted)
	if err != nil {
		return Meeting{}, err
	}
	m.Start = time.Unix(0, startNS)
	return m, nil
}

// Get returns the meeting that started at start. The window never needed
// this -- it already had the row it was showing -- but a caller that is
// handed only an id does.
func (s *MeetingStore) Get(start time.Time) (Meeting, error) {
	db, err := s.open()
	if err != nil {
		return Meeting{}, err
	}
	m, err := scanMeeting(db.sql.QueryRow(selectMeetingSQL+" WHERE start_ns = ?", start.UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return Meeting{}, fmt.Errorf("history: no meeting at %s", start.Format(time.RFC3339Nano))
	}
	if err != nil {
		return Meeting{}, fmt.Errorf("history: reading meeting: %w", err)
	}
	return m, nil
}

// All returns every meeting, newest first.
func (s *MeetingStore) All() ([]Meeting, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}

	rows, err := db.sql.Query(selectMeetingSQL + " ORDER BY start_ns DESC")
	if err != nil {
		return nil, fmt.Errorf("history: reading meetings: %w", err)
	}
	defer rows.Close()

	var all []Meeting
	for rows.Next() {
		m, err := scanMeeting(rows)
		if err != nil {
			return nil, fmt.Errorf("history: reading meetings: %w", err)
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

// readMeetingFile decodes one of the old per-meeting JSON records. Only the
// migration calls it: those files are a frozen backup after the move to the
// database, never read in normal operation again.
func readMeetingFile(path string) (Meeting, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Meeting{}, err
	}
	var m Meeting
	if err := json.Unmarshal(raw, &m); err != nil {
		return Meeting{}, err
	}
	return m, nil
}
