package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTaskFile(t *testing.T, dir string, tk Task) {
	t.Helper()
	raw, err := json.Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tk.ID+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateFilesImportsTasksAndTheRejectedList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Tasks")
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.Local)
	reminder := created.Add(2 * time.Hour)
	writeTaskFile(t, dir, Task{
		ID: "20260820-100000.000000000", Text: "send the invoice", Entity: "Northwind",
		Status: StatusTodo, Created: created, Reminder: &reminder,
		SourceKind: "meeting", SourceKey: created.Format(time.RFC3339Nano), Notes: "before Friday",
	})
	writeTaskFile(t, dir, Task{
		ID: "20260820-110000.000000000", Text: "already done", Status: StatusDone,
		Created: created.Add(time.Hour),
	})
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "task-rejected.json"),
		[]byte(`["not a task at all"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(dir)
	sentinel := filepath.Join(t.TempDir(), "migrated-tasks")
	n, err := s.MigrateFiles(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("imported %d tasks, want 2", n)
	}

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	// Newest first, as the pane has always been handed them.
	if len(all) != 2 || all[0].Text != "already done" || all[1].Text != "send the invoice" {
		t.Fatalf("got %+v, want newest first", all)
	}
	got := all[1]
	if got.Entity != "Northwind" || got.Notes != "before Friday" || got.SourceKind != "meeting" {
		t.Fatalf("got %+v, want every field carried across", got)
	}
	if got.Reminder == nil || !got.Reminder.Equal(reminder) {
		t.Fatalf("reminder = %v, want %v", got.Reminder, reminder)
	}
	// A task nobody set a reminder on, or has edited, must not come back
	// claiming 1970.
	if all[0].Reminder != nil || !all[0].Updated.IsZero() {
		t.Fatalf("got %+v, want no reminder and no edit time", all[0])
	}

	list, err := s.LoadRejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != "not a task at all" {
		t.Fatalf("got %v, want the rejected line imported", list)
	}

	// Re-running is a no-op, and the files stay: a frozen backup for one
	// release, deletable by the user, re-importable by deleting the sentinel.
	if n, err := s.MigrateFiles(sentinel); err != nil || n != 0 {
		t.Fatalf("second run imported %d (err %v), want 0", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "20260820-100000.000000000.json")); err != nil {
		t.Fatalf("the task file was removed: %v", err)
	}
	if again, _ := s.MigrateFiles(filepath.Join(t.TempDir(), "fresh")); again != 2 {
		t.Fatalf("a forced re-run imported %d, want 2", again)
	}
	if all, _ := s.All(); len(all) != 2 {
		t.Fatalf("a re-run duplicated tasks: %d rows", len(all))
	}
}
