// Package task stores tasks the LLM classifier pulled out of a dictation or
// meeting transcript. They are rows in the same database meetings and
// dictations live in (internal/history) -- one file per task was what this
// started as, which made "every task, newest first" a directory glob and a
// parse per row, and left no way to ask for the tasks of one project without
// reading all of them.
//
// The tables are declared in history's schema steps, and this package talks to
// them over history.DB.SQL(). One database means one migration history and one
// file to back up; see the comment on that method for why it is that way round.
package task

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// Store is the tasks half of the app's storage. Its public surface is
// unchanged from the one-file-per-task version; the mutex is gone with the
// read-modify-write it guarded, since the database is opened with
// SetMaxOpenConns(1) and every method here is one statement or one
// transaction.
type Store struct {
	dir string

	// Opened on first use, like the history stores: the app builds its stores
	// before it knows whether anything is writable.
	once    sync.Once
	db      *history.DB
	openErr error
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

// NewStoreDB is the app's constructor: it hands over the already-open
// database, which lives beside the settings rather than in dir -- dir is the
// user's transcripts folder, and may well be synced.
func NewStoreDB(dir string, db *history.DB) *Store {
	s := NewStore(dir)
	s.once.Do(func() { s.db = db })
	return s
}

// Dir is where the old per-task files are. Still the migration's input, and
// still where rejectedPath's sibling file sat.
func (s *Store) Dir() string { return s.dir }

func (s *Store) sql() (*sql.DB, error) {
	s.once.Do(func() {
		s.db, s.openErr = history.OpenDB(filepath.Join(s.dir, "voxlog.db"))
	})
	if s.openErr != nil {
		return nil, s.openErr
	}
	return s.db.SQL(), nil
}

// nsOrNil and timeFromNS carry the two optional times. NULL rather than zero:
// a reminder nobody set and an edit nobody made are absences, and a zero
// time.Time stored as nanoseconds reads back as 1970.
func nsOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

func timeFromNS(ns *int64) time.Time {
	if ns == nil {
		return time.Time{}
	}
	return time.Unix(0, *ns)
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

// Append writes one task, replacing any row already under that id. Replacing
// rather than failing: a re-run migration re-states what it already knows.
func (s *Store) Append(t Task) error {
	db, err := s.sql()
	if err != nil {
		return err
	}
	var reminder any
	if t.Reminder != nil {
		reminder = nsOrNil(*t.Reminder)
	}
	_, err = db.Exec(`
		INSERT INTO tasks
			(id, source_kind, source_key, text, entity, status, notes, reminder_ns, created_ns, updated_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			source_kind = excluded.source_kind,
			source_key  = excluded.source_key,
			text        = excluded.text,
			entity      = excluded.entity,
			status      = excluded.status,
			notes       = excluded.notes,
			reminder_ns = excluded.reminder_ns,
			created_ns  = excluded.created_ns,
			updated_ns  = excluded.updated_ns`,
		t.ID, t.SourceKind, t.SourceKey, t.Text, t.Entity, string(t.Status),
		t.Notes, reminder, t.Created.UnixNano(), nsOrNil(t.Updated))
	if err != nil {
		return fmt.Errorf("task: appending %s: %w", t.ID, err)
	}
	return nil
}

// Update applies mutate to the task with id and writes it back -- how a
// status pill click reaches disk.
//
// The read and the write are one immediate transaction, so a status click and
// a notes edit arriving together cannot splice their fields.
func (s *Store) Update(id string, mutate func(*Task)) error {
	db, err := s.sql()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("task: updating %s: %w", id, err)
	}
	defer tx.Rollback()

	t, err := scanTask(tx.QueryRow(selectTaskSQL+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: no task %s", id)
	}
	if err != nil {
		return fmt.Errorf("task: reading %s: %w", id, err)
	}
	mutate(&t)

	var reminder any
	if t.Reminder != nil {
		reminder = nsOrNil(*t.Reminder)
	}
	_, err = tx.Exec(`
		UPDATE tasks SET
			source_kind = ?, source_key = ?, text = ?, entity = ?, status = ?,
			notes = ?, reminder_ns = ?, created_ns = ?, updated_ns = ?
		WHERE id = ?`,
		t.SourceKind, t.SourceKey, t.Text, t.Entity, string(t.Status),
		t.Notes, reminder, t.Created.UnixNano(), nsOrNil(t.Updated), id)
	if err != nil {
		return fmt.Errorf("task: writing %s: %w", id, err)
	}
	return tx.Commit()
}

const selectTaskSQL = `
	SELECT id, source_kind, source_key, text, entity, status, notes,
	       reminder_ns, created_ns, updated_ns
	FROM tasks`

type rowScanner interface{ Scan(dest ...any) error }

func scanTask(row rowScanner) (Task, error) {
	var (
		t                     Task
		status                string
		reminderNS, updatedNS *int64
		createdNS             int64
	)
	err := row.Scan(&t.ID, &t.SourceKind, &t.SourceKey, &t.Text, &t.Entity,
		&status, &t.Notes, &reminderNS, &createdNS, &updatedNS)
	if err != nil {
		return Task{}, err
	}
	t.Status = Status(status)
	t.Created = time.Unix(0, createdNS)
	t.Updated = timeFromNS(updatedNS)
	if reminderNS != nil {
		at := time.Unix(0, *reminderNS)
		t.Reminder = &at
	}
	return t, nil
}

// Get reads one task by id.
func (s *Store) Get(id string) (Task, error) {
	db, err := s.sql()
	if err != nil {
		return Task{}, err
	}
	t, err := scanTask(db.QueryRow(selectTaskSQL+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task: no task %s", id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("task: reading %s: %w", id, err)
	}
	return t, nil
}

// Delete removes a task's file. Missing is not an error -- deleting
// something already gone still ends with the task not existing, which is
// all a caller actually cares about.
func (s *Store) Delete(id string) error {
	db, err := s.sql()
	if err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM tasks WHERE id = ?", id); err != nil {
		return fmt.Errorf("task: deleting %s: %w", id, err)
	}
	return nil
}

// All returns every task, newest first -- the order the Tasks pane and the
// MCP tools have always been handed them in.
func (s *Store) All() ([]Task, error) {
	db, err := s.sql()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(selectTaskSQL + " ORDER BY created_ns DESC")
	if err != nil {
		return nil, fmt.Errorf("task: listing: %w", err)
	}
	defer rows.Close()

	all := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("task: listing: %w", err)
		}
		all = append(all, t)
	}
	return all, rows.Err()
}

// EntityNames returns the distinct entity names already in use, so the
// classifier prompt can ask the model to reuse one instead of inventing a
// near-duplicate for the same project or client.
//
// Folded in Go rather than with SQL's DISTINCT: SQLite's NOCASE collation only
// folds ASCII, and these names are routinely Cyrillic.
func (s *Store) EntityNames() ([]string, error) {
	db, err := s.sql()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT entity FROM tasks WHERE entity <> '' ORDER BY created_ns DESC")
	if err != nil {
		return nil, fmt.Errorf("task: listing projects: %w", err)
	}
	defer rows.Close()

	seen := map[string]string{} // casefold -> first-seen spelling
	for rows.Next() {
		var entity string
		if err := rows.Scan(&entity); err != nil {
			return nil, fmt.Errorf("task: listing projects: %w", err)
		}
		key := strings.ToLower(entity)
		if _, ok := seen[key]; !ok {
			seen[key] = entity
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(seen))
	for _, name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
