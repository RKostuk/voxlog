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

// A meeting is one file, named for when it started. Meetings arrive a few a
// day rather than dozens an hour, and each carries fields a dictation never
// will -- two audio tracks and a length measured in hours -- so grouping them
// into the day files alongside dictations is what forced the Kind
// discriminator this store exists to retire.
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
}

type MeetingStore struct {
	dir string
	// mu serializes the read-modify-write in Update, and the create in
	// Append. One file per meeting already keeps two different meetings out
	// of each other's way; this is for two writers touching the SAME
	// meeting, which is exactly what a transcript landing while a retention
	// sweep clears the audio path would be.
	mu sync.Mutex
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

// Dir is where meeting records live. Their audio lives beside them under the
// same start-time stamp, same as day files and dictation audio do.
func (s *MeetingStore) Dir() string { return s.dir }

func (s *MeetingStore) meetingPath(start time.Time) string {
	return filepath.Join(s.dir, start.Format("2006-01-02-150405")+".json")
}

func (s *MeetingStore) readMeeting(path string) (Meeting, error) {
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

func (s *MeetingStore) writeMeeting(path string, m Meeting) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename, same as writeDay: a crash mid-write must never leave
	// a half-written file in the real path. For Append that would only cost
	// the new record; for Update it would destroy a file that may already
	// hold a transcript won minutes earlier.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *MeetingStore) Append(m Meeting) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.writeMeeting(s.meetingPath(m.Start), m)
}

// Update applies mutate to the meeting starting at start and writes it back.
// This is how a transcript reaches a meeting that was recorded before it
// existed: the meeting lands in history the moment the call ends, and the
// text arrives minutes later when its decode finishes.
func (s *MeetingStore) Update(start time.Time, mutate func(*Meeting)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.meetingPath(start)
	m, err := s.readMeeting(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("history: no meeting at %s", start.Format(time.RFC3339Nano))
		}
		return fmt.Errorf("history: reading meeting %s: %w", path, err)
	}
	mutate(&m)
	return s.writeMeeting(path, m)
}

// All reads every meeting file and returns them newest first. A file that
// will not parse is skipped and logged rather than failing the whole read --
// a meeting the user can still see is worth more than a strict parse, and a
// corrupt neighbor should not take the good ones down with it.
func (s *MeetingStore) All() ([]Meeting, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}

	// Keyed by UnixNano rather than by Meeting.Start itself: two time.Time
	// values decoded from separate JSON reads can hold the same instant but
	// compare unequal under == (their internal wall/monotonic layout is not
	// guaranteed identical), which would silently defeat the dedup below.
	// UnixNano collapses that down to a plain comparable integer.
	byStart := make(map[int64]Meeting, len(files))
	for _, f := range files {
		m, err := s.readMeeting(f)
		if err != nil {
			log.Printf("history: skipping corrupt meeting file %s: %v", f, err)
			continue
		}
		// A crashed migration (Task 2) can leave two files claiming the same
		// Start. Keep the one with a transcript, since that is strictly more
		// of the meeting recovered; if neither or both have one, the choice
		// is arbitrary, so just keep whichever was seen first.
		key := m.Start.UnixNano()
		if existing, ok := byStart[key]; ok {
			if existing.Text != "" || m.Text == "" {
				continue
			}
		}
		byStart[key] = m
	}

	all := make([]Meeting, 0, len(byStart))
	for _, m := range byStart {
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Start.After(all[j].Start)
	})
	return all, nil
}
