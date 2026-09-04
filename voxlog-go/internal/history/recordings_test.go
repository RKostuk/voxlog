package history

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write puts a recording of the given size in dir, aged by backdating it.
func writeRecording(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSweepRemovesOnlyWhatIsPastTheCutoff(t *testing.T) {
	dir := t.TempDir()
	old := writeRecording(t, dir, "old.wav", 100, 10*24*time.Hour)
	fresh := writeRecording(t, dir, "fresh.wav", 100, 2*24*time.Hour)

	removed, freed, err := SweepRecordings(dir, RetentionWeek, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != 100 {
		t.Fatalf("removed %d freeing %d, want 1 and 100", removed, freed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the recording past the cutoff is still there")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recording inside the window was deleted")
	}
}

// An unknown policy must read as "keep everything". A corrupt settings file
// is not permission to start deleting.
func TestSweepDoesNothingWhenBothPoliciesAreOff(t *testing.T) {
	dir := t.TempDir()
	writeRecording(t, dir, "a.wav", 100, 400*24*time.Hour)

	for _, policy := range []string{RetentionDisabled, "", "nonsense"} {
		removed, freed, err := SweepRecordings(dir, policy, 0, nil)
		if err != nil || removed != 0 || freed != 0 {
			t.Errorf("policy %q: removed %d freeing %d (%v), want nothing touched", policy, removed, freed, err)
		}
	}
}

func TestSweepTrimsToTheCeilingOldestFirst(t *testing.T) {
	dir := t.TempDir()
	// 3 x 100 bytes, distinct ages. A 250-byte ceiling has room for two.
	writeRecording(t, dir, "oldest.wav", 100, 3*time.Hour)
	writeRecording(t, dir, "middle.wav", 100, 2*time.Hour)
	writeRecording(t, dir, "newest.wav", 100, 1*time.Hour)

	removed, freed, err := SweepRecordings(dir, RetentionDisabled, 250, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != 100 {
		t.Fatalf("removed %d freeing %d, want 1 and 100", removed, freed)
	}
	if _, err := os.Stat(filepath.Join(dir, "oldest.wav")); !os.IsNotExist(err) {
		t.Error("the ceiling was met by deleting something other than the oldest recording")
	}
	for _, keep := range []string{"middle.wav", "newest.wav"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s should have survived: %v", keep, err)
		}
	}
}

func TestSweepStopsAsSoonAsItFits(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		writeRecording(t, dir, fmt.Sprintf("r%d.wav", i), 100, time.Duration(5-i)*time.Hour)
	}

	removed, _, err := SweepRecordings(dir, RetentionDisabled, 300, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d, want exactly the 2 needed to reach 300 bytes", removed)
	}
	if size, _ := RecordingsSize(dir); size > 300 {
		t.Fatalf("folder is %d bytes, want it under the ceiling", size)
	}
}

// A meeting that is still recording holds its WAV open and is still growing
// it. Deleting that file mid-call is the one unrecoverable thing this code
// could do.
func TestSweepNeverTouchesARecordingInUse(t *testing.T) {
	dir := t.TempDir()
	live := writeRecording(t, dir, "live-mic.wav", 100, 400*24*time.Hour)
	writeRecording(t, dir, "done.wav", 100, 400*24*time.Hour)

	removed, _, err := SweepRecordings(dir, RetentionWeek, 0, map[string]bool{live: true})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want only the finished recording", removed)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("the recording still being written was deleted")
	}
}

// Both policies at once: age first, then the ceiling on what is left.
func TestSweepAppliesAgeThenCeiling(t *testing.T) {
	dir := t.TempDir()
	writeRecording(t, dir, "ancient.wav", 100, 40*24*time.Hour)
	writeRecording(t, dir, "old.wav", 100, 3*time.Hour)
	writeRecording(t, dir, "new.wav", 100, 1*time.Hour)

	removed, freed, err := SweepRecordings(dir, RetentionMonth, 150, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 || freed != 200 {
		t.Fatalf("removed %d freeing %d, want 2 and 200", removed, freed)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.wav")); err != nil {
		t.Error("the newest recording should have survived both passes")
	}
}

// The folder holds recordings. Anything else in it belongs to someone else.
func TestSweepIgnoresWhatIsNotARecording(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "notes.txt")
	when := time.Now().Add(-400 * 24 * time.Hour)
	if err := os.Chtimes(old, when, when); err != nil {
		t.Fatal(err)
	}

	removed, _, err := SweepRecordings(dir, RetentionWeek, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d, want 0 -- only .wav files are this sweep's business", removed)
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("a file that is not a recording was deleted")
	}
}

func TestRecordingsSizeCountsOnlyRecordings(t *testing.T) {
	dir := t.TempDir()
	writeRecording(t, dir, "a.wav", 100, time.Hour)
	writeRecording(t, dir, "b.wav", 250, time.Hour)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), make([]byte, 999), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := RecordingsSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != 350 {
		t.Fatalf("got %d, want 350", got)
	}
}

func TestRecordingsSizeOfAMissingFolderIsZero(t *testing.T) {
	got, err := RecordingsSize(filepath.Join(t.TempDir(), "not-there"))
	if err != nil || got != 0 {
		t.Fatalf("got %d, %v; want 0 and no error", got, err)
	}
}
