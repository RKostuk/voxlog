package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// maxRejected bounds the negative-example list -- kept small on purpose so
// the prompt it feeds into (see llm.Classify) never grows by more than a
// handful of short lines, no matter how many "Not a task" corrections pile
// up over the app's lifetime. Oldest drops off as new ones come in.
const maxRejected = 8

// rejectedPath is where this list lived when it was a file: beside the Tasks
// directory, not inside it, because Store.All used to glob every *.json in
// s.dir and would have tried to parse this as a Task. Still read once, by the
// migration.
func (s *Store) rejectedPath() string {
	return filepath.Join(filepath.Dir(s.dir), "task-rejected.json")
}

// LoadRejected returns the extracted task text of everything the user has
// explicitly marked "Not a task" (as opposed to a plain Delete, which
// records nothing -- see main_window.go's setTaskStatus/deleteTask/
// rejectTask bindings), oldest first.
func (s *Store) LoadRejected() ([]string, error) {
	db, err := s.sql()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT text FROM task_rejected ORDER BY seq")
	if err != nil {
		return nil, fmt.Errorf("task: reading the rejected list: %w", err)
	}
	defer rows.Close()

	var list []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("task: reading the rejected list: %w", err)
		}
		list = append(list, text)
	}
	return list, rows.Err()
}

// RemoveRejected drops one entry from the list. Removing an entry that is
// not there is not an error: the caller is a UI button, and a stale click
// after the list has already changed under it should be a no-op, not a
// failure dialog.
func (s *Store) RemoveRejected(text string) error {
	db, err := s.sql()
	if err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM task_rejected WHERE text = ?", text); err != nil {
		return fmt.Errorf("task: removing from the rejected list: %w", err)
	}
	return nil
}

// AppendRejected records one more misclassification and trims the list back
// to maxRejected, oldest first out. Re-rejecting the same line moves it to
// the end rather than storing it twice -- text is unique in the table.
func (s *Store) AppendRejected(text string) error {
	db, err := s.sql()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("task: appending to the rejected list: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM task_rejected WHERE text = ?", text); err != nil {
		return fmt.Errorf("task: appending to the rejected list: %w", err)
	}
	if _, err := tx.Exec("INSERT INTO task_rejected (text) VALUES (?)", text); err != nil {
		return fmt.Errorf("task: appending to the rejected list: %w", err)
	}
	// Trim by seq, which is the insertion order: the oldest examples are the
	// least like whatever the classifier is getting wrong today.
	if _, err := tx.Exec(`
		DELETE FROM task_rejected
		WHERE seq NOT IN (SELECT seq FROM task_rejected ORDER BY seq DESC LIMIT ?)`, maxRejected); err != nil {
		return fmt.Errorf("task: trimming the rejected list: %w", err)
	}
	return tx.Commit()
}

// MigrateFiles imports the per-task JSON files and the rejected list into the
// database, once. Same shape as history's migrations: a sentinel, safe to
// re-run, and the old files are deliberately left on disk as a frozen backup
// for one release.
func (s *Store) MigrateFiles(sentinelPath string) (int, error) {
	if _, err := os.Stat(sentinelPath); err == nil {
		return 0, nil
	}

	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, f := range files {
		t, err := s.readTask(f)
		if err != nil {
			// One unreadable file must not block the import, and with it the
			// sentinel, on every future launch.
			continue
		}
		if t.ID == "" {
			continue
		}
		if err := s.Append(t); err != nil {
			return moved, fmt.Errorf("task: migrating %s: %w", f, err)
		}
		moved++
	}

	if raw, err := os.ReadFile(s.rejectedPath()); err == nil {
		var list []string
		if err := json.Unmarshal(raw, &list); err == nil {
			for _, text := range list {
				if err := s.AppendRejected(text); err != nil {
					return moved, fmt.Errorf("task: migrating the rejected list: %w", err)
				}
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(sentinelPath), 0o755); err != nil {
		return moved, err
	}
	line := fmt.Sprintf("imported %d task(s)\n", moved)
	if err := os.WriteFile(sentinelPath, []byte(line), 0o644); err != nil {
		return moved, err
	}
	return moved, nil
}
