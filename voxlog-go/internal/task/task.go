// Package task stores tasks the LLM classifier pulled out of a dictation or
// meeting transcript -- one JSON file per task, so a status change never has
// to rewrite a file holding anyone else's task.
package task

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"voxlog-go/internal/history"
)

type Status string

const (
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusDone       Status = "done"
)

type Task struct {
	ID string `json:"id"`
	// SourceKind is history.KindDictation or history.KindMeeting; SourceKey
	// is that entry's Timestamp (dictation) or Start (meeting), formatted
	// RFC3339Nano -- the same ID scheme the History/Meetings windows already
	// round-trip an entry through, so linking back costs nothing new.
	SourceKind string     `json:"source_kind"`
	SourceKey  string     `json:"source_key"`
	Text       string     `json:"text"`
	Entity     string     `json:"entity"`
	Status     Status     `json:"status"`
	Reminder   *time.Time `json:"reminder,omitempty"`
	Created    time.Time  `json:"created"`
	// Notes is the user's own free text about the task -- everything the
	// classifier's one-line extraction had no room for. Updated is when the
	// text or the notes were last edited by hand; zero on a task nobody has
	// touched since it was classified. Both omitempty, so tasks written
	// before these existed decode unchanged.
	Notes   string    `json:"notes,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func DefaultDir() string {
	return filepath.Join(history.DefaultDir(), "Tasks")
}

func NewStore(dir string) *Store {
	if dir == "" {
		dir = DefaultDir()
	}
	return &Store{dir: dir}
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) taskPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// NewID is time-sortable, like the meeting-file naming scheme, but keeps
// nanoseconds since more than one task can be created in the same second (a
// meeting transcript can yield several).
func NewID() string {
	return time.Now().UTC().Format("20060102-150405.000000000")
}

func (s *Store) readTask(path string) (Task, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Task{}, err
	}
	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return Task{}, err
	}
	return t, nil
}

func (s *Store) writeTask(path string, t Task) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) Append(t Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.writeTask(s.taskPath(t.ID), t)
}

// Update applies mutate to the task with id and writes it back -- how a
// status pill click reaches disk.
func (s *Store) Update(id string, mutate func(*Task)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.taskPath(id)
	t, err := s.readTask(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("task: no task %s", id)
		}
		return fmt.Errorf("task: reading %s: %w", path, err)
	}
	mutate(&t)
	return s.writeTask(path, t)
}

// Get reads one task by id.
func (s *Store) Get(id string) (Task, error) {
	return s.readTask(s.taskPath(id))
}

// Delete removes a task's file. Missing is not an error -- deleting
// something already gone still ends with the task not existing, which is
// all a caller actually cares about.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.taskPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// All reads every task file, newest first. A file that will not parse is
// skipped and logged, same as MeetingStore.All -- one corrupt task should
// not hide the rest of the list.
func (s *Store) All() ([]Task, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}

	all := make([]Task, 0, len(files))
	for _, f := range files {
		t, err := s.readTask(f)
		if err != nil {
			log.Printf("task: skipping corrupt task file %s: %v", f, err)
			continue
		}
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Created.After(all[j].Created)
	})
	return all, nil
}

// EntityNames returns the distinct entity names already in use, so the
// classifier prompt can ask the model to reuse one instead of inventing a
// near-duplicate for the same project or client.
func (s *Store) EntityNames() ([]string, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]string, len(all)) // casefold -> first-seen spelling
	for _, t := range all {
		if t.Entity == "" {
			continue
		}
		key := strings.ToLower(t.Entity)
		if _, ok := seen[key]; !ok {
			seen[key] = t.Entity
		}
	}
	names := make([]string, 0, len(seen))
	for _, name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
