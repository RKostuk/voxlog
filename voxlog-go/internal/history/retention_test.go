package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDayFile(t *testing.T, dir string, day time.Time) string {
	t.Helper()
	path := filepath.Join(dir, day.Format("2006-01-02")+".json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneDisabledRemovesNothing(t *testing.T) {
	dir := t.TempDir()
	old := writeDayFile(t, dir, time.Now().AddDate(0, 0, -365))

	s := NewStore(dir)
	removed, err := s.Prune(RetentionDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d files, want 0", removed)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("old file should still exist: %v", err)
	}
}

func TestPruneUnknownPolicyRemovesNothing(t *testing.T) {
	dir := t.TempDir()
	writeDayFile(t, dir, time.Now().AddDate(0, 0, -365))

	s := NewStore(dir)
	removed, err := s.Prune("not-a-real-policy")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d files, want 0 -- an unknown policy must never delete", removed)
	}
}

func TestPruneRemovesOnlyEntriesPastCutoff(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	recent := time.Now().AddDate(0, 0, -2)
	old := time.Now().AddDate(0, 0, -30)
	for _, at := range []time.Time{recent, old} {
		if err := s.Append(Entry{Timestamp: at, Text: "take"}); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := s.Prune(RetentionWeek)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d entries, want 1", removed)
	}
	left, err := s.AllEntries()
	if err != nil {
		t.Fatal(err)
	}
	// One entry at a time now, not one day at a time: half a day past the
	// cutoff used to be kept because a day was the unit on disk.
	if len(left) != 1 || !left[0].Timestamp.Equal(recent) {
		t.Fatalf("got %+v, want only the two-day-old take", left)
	}
}

// Day files are last release's copy of what is now in the database, and the
// retention setting covers them too.
func TestPruneAlsoSweepsLeftoverDayFiles(t *testing.T) {
	dir := t.TempDir()
	recent := writeDayFile(t, dir, time.Now().AddDate(0, 0, -2))
	old := writeDayFile(t, dir, time.Now().AddDate(0, 0, -30))

	if _, err := NewStore(dir).Prune(RetentionWeek); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent file should survive a 1-week policy: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("30-day-old file should be gone under a 1-week policy, stat err = %v", err)
	}
}

func TestPruneLeavesNonDayFilesAlone(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "notes.json")
	if err := os.WriteFile(stray, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(dir)
	if _, err := s.Prune(RetentionWeek); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("non-day .json should be untouched: %v", err)
	}
}
