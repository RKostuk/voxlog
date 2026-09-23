package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestAppendThenEntriesForDay(t *testing.T) {
	s := NewStore(t.TempDir())
	e := Entry{
		Timestamp:        mustParse(t, "2026-08-12T10:00:00Z"),
		DurationSeconds:  1.2,
		Text:             "hello world",
		RecordingSeconds: 1.0,
	}
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.EntriesForDay(e.Timestamp)
	if err != nil {
		t.Fatalf("EntriesForDay: %v", err)
	}
	if len(got) != 1 || got[0].Text != "hello world" {
		t.Fatalf("got %+v", got)
	}
}

func TestEntriesForDayEmptyWhenNoFile(t *testing.T) {
	s := NewStore(t.TempDir())
	got, err := s.EntriesForDay(mustParse(t, "2026-01-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("EntriesForDay: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want 0", len(got))
	}
}

func TestAllEntriesSpansMultipleDaysOldestFirst(t *testing.T) {
	s := NewStore(t.TempDir())
	later := Entry{Timestamp: mustParse(t, "2026-08-13T09:00:00Z"), Text: "second day"}
	earlier := Entry{Timestamp: mustParse(t, "2026-08-12T09:00:00Z"), Text: "first day"}

	if err := s.Append(later); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(earlier); err != nil {
		t.Fatal(err)
	}

	got, err := s.AllEntries()
	if err != nil {
		t.Fatalf("AllEntries: %v", err)
	}
	if len(got) != 2 || got[0].Text != "first day" || got[1].Text != "second day" {
		t.Fatalf("got %+v, want [first day, second day] in that order", got)
	}
}

func TestAppendMultipleEntriesSameDay(t *testing.T) {
	s := NewStore(t.TempDir())
	day := mustParse(t, "2026-08-12T08:00:00Z")
	e1 := Entry{Timestamp: day, Text: "one"}
	e2 := Entry{Timestamp: day.Add(time.Hour), Text: "two"}
	if err := s.Append(e1); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(e2); err != nil {
		t.Fatal(err)
	}

	got, err := s.EntriesForDay(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
}

// A leftover day file -- corrupt or otherwise -- is last release's copy of
// data that now lives in the database. The list must come from the rows and
// not go looking at it.
func TestAllEntriesIgnoresLeftoverDayFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	good := Entry{Timestamp: mustParse(t, "2026-08-12T09:00:00Z"), Text: "good"}
	if err := s.Append(good); err != nil {
		t.Fatal(err)
	}

	// A corrupt leftover file beside the database.
	corruptPath := filepath.Join(dir, "2026-08-13.json")
	if err := os.WriteFile(corruptPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.AllEntries()
	if err != nil {
		t.Fatalf("AllEntries should not fail on a corrupt sibling file: %v", err)
	}
	if len(got) != 1 || got[0].Text != "good" {
		t.Fatalf("got %+v, want just the good entry", got)
	}
}

// A dictation has to still be there in the next process. The store used to
// prove this by rewriting a whole day file atomically through a temp file and
// a rename; now it is a committed row, and what is worth checking is that a
// fresh store over the same directory sees it.
func TestAnAppendSurvivesReopeningTheStore(t *testing.T) {
	dir := t.TempDir()
	e := Entry{Timestamp: mustParse(t, "2026-08-12T10:00:00Z"), Text: "durable", RecordingSeconds: 2}
	if err := NewStore(dir).Append(e); err != nil {
		t.Fatal(err)
	}

	got, err := NewStore(dir).EntriesForDay(e.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "durable" || got[0].RecordingSeconds != 2 {
		t.Fatalf("got %+v, want the entry a previous store wrote", got)
	}
}
