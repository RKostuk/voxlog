package history

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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

type Store struct {
	dir string
	// mu serializes the read-modify-write in Append and Update. Several
	// transcripts can now finish at once (each take decodes on its own
	// schedule), and two concurrent appends to the same day file would each
	// read the same list and write it back with only their own entry added --
	// silently losing one transcript. A second *process* is still out of
	// scope, exactly as it was before.
	mu sync.Mutex
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

// Dir is where the day files live. Meeting recordings are kept alongside
// them, so the audio and the transcript it produced travel together and one
// retention policy covers both.
func (s *Store) Dir() string { return s.dir }

func (s *Store) dayPath(day time.Time) string {
	return filepath.Join(s.dir, day.Format("2006-01-02")+".json")
}

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

func (s *Store) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.dayPath(e.Timestamp)
	entries, err := s.readDay(path)
	if err != nil {
		return err
	}
	entries = append(entries, e)
	return s.writeDay(path, entries)
}

// Update applies mutate to the entry with the given timestamp and writes the
// day file back. This is how a transcript reaches an entry that was written
// before it existed: a meeting lands in history the moment it stops, and the
// text arrives minutes later when its decode finishes.
//
// Timestamps identify entries here because they are what the History window
// already keys on, and two takes cannot start in the same nanosecond.
func (s *Store) Update(timestamp time.Time, mutate func(*Entry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.dayPath(timestamp)
	entries, err := s.readDay(path)
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].Timestamp.Equal(timestamp) {
			mutate(&entries[i])
			return s.writeDay(path, entries)
		}
	}
	return fmt.Errorf("history: no entry at %s", timestamp.Format(time.RFC3339Nano))
}

func (s *Store) EntriesForDay(day time.Time) ([]Entry, error) {
	entries, err := s.readDay(s.dayPath(day))
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *Store) AllEntries() ([]Entry, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files) // filenames are YYYY-MM-DD.json, lexical == chronological

	var all []Entry
	for _, f := range files {
		entries, err := s.readDay(f)
		if err != nil {
			log.Printf("history: skipping corrupt day file %s: %v", f, err)
			continue
		}
		all = append(all, entries...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.Before(all[j].Timestamp)
	})
	return all, nil
}
