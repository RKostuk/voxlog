package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeRawDayFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationMovesMeetingsOutOfADayFile(t *testing.T) {
	dayDir, meetDir := t.TempDir(), t.TempDir()
	writeRawDayFile(t, dayDir, "2026-08-01.json", `[
	  {"timestamp":"2026-08-01T09:00:00Z","duration_seconds":1.5,"text":"a dictation","recording_seconds":3},
	  {"timestamp":"2026-08-01T11:00:00Z","kind":"meeting","recording_seconds":2531,"text":"a call","duration_seconds":9,"audio_path":"/tmp/m-mic.wav"}
	]`)

	days, meetings := NewStore(dayDir), NewMeetingStore(meetDir)
	sentinel := filepath.Join(t.TempDir(), "migrated")

	n, err := MigrateMeetings(days, meetings, sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("moved %d meetings, want 1", n)
	}

	got, _ := meetings.All()
	if len(got) != 1 || got[0].Text != "a call" || got[0].RecordingSeconds != 2531 ||
		got[0].DurationSeconds != 9 || got[0].AudioPath != "/tmp/m-mic.wav" {
		t.Fatalf("got %+v, want every field carried across", got)
	}

	// The dictation stays in the day file: this migration only moves meetings,
	// and MigrateDictations is what imports the rest of it later.
	left, _ := days.readDay(filepath.Join(dayDir, "2026-08-01.json"))
	if len(left) != 1 || left[0].Text != "a dictation" {
		t.Fatalf("got %+v, want the dictation left alone in the day file", left)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("the sentinel was not written: %v", err)
	}
}

// A day file written before meetings existed has no "kind" field at all. It
// must come through untouched -- and the file must not even be rewritten,
// since rewriting it risks losing data for no gain.
func TestMigrationLeavesAPreMeetingDayFileAlone(t *testing.T) {
	dayDir, meetDir := t.TempDir(), t.TempDir()
	raw := `[{"timestamp":"2026-08-01T09:00:00Z","duration_seconds":1.5,"text":"hello","recording_seconds":3}]`
	writeRawDayFile(t, dayDir, "2026-08-01.json", raw)
	before, err := os.Stat(filepath.Join(dayDir, "2026-08-01.json"))
	if err != nil {
		t.Fatal(err)
	}

	n, err := MigrateMeetings(NewStore(dayDir), NewMeetingStore(meetDir), filepath.Join(t.TempDir(), "migrated"))
	if err != nil || n != 0 {
		t.Fatalf("moved %d, %v; want 0 and no error", n, err)
	}

	after, _ := os.Stat(filepath.Join(dayDir, "2026-08-01.json"))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("a day file with no meetings in it was rewritten anyway")
	}
	got, _ := os.ReadFile(filepath.Join(dayDir, "2026-08-01.json"))
	if string(got) != raw {
		t.Errorf("the day file changed:\n got %s\nwant %s", got, raw)
	}
}

// The sentinel is what stops a second run. Without it, a meeting the user has
// since edited or deleted would come back from the day file it was already
// removed from.
func TestMigrationRunsOnlyOnce(t *testing.T) {
	dayDir, meetDir := t.TempDir(), t.TempDir()
	writeRawDayFile(t, dayDir, "2026-08-01.json",
		`[{"timestamp":"2026-08-01T11:00:00Z","kind":"meeting","recording_seconds":60}]`)
	sentinel := filepath.Join(t.TempDir(), "migrated")

	if n, err := MigrateMeetings(NewStore(dayDir), NewMeetingStore(meetDir), sentinel); err != nil || n != 1 {
		t.Fatalf("first run moved %d, %v; want 1", n, err)
	}
	if n, err := MigrateMeetings(NewStore(dayDir), NewMeetingStore(meetDir), sentinel); err != nil || n != 0 {
		t.Fatalf("second run moved %d, %v; want 0 -- the sentinel did not hold", n, err)
	}

	got, _ := NewMeetingStore(meetDir).All()
	if len(got) != 1 {
		t.Fatalf("got %d meetings after two runs, want 1", len(got))
	}
}

// Meeting files are written before the day file is rewritten, so a crash in
// between leaves a duplicate rather than a hole. Re-running must converge on
// one meeting, not two.
func TestMigrationAfterAPartialRunDoesNotDuplicate(t *testing.T) {
	dayDir, meetDir := t.TempDir(), t.TempDir()
	writeRawDayFile(t, dayDir, "2026-08-01.json",
		`[{"timestamp":"2026-08-01T11:00:00Z","kind":"meeting","recording_seconds":60,"text":"a call"}]`)

	// Stand in for the crashed first attempt: the meeting file exists, the
	// day file was never rewritten, and no sentinel was left.
	at := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	if err := NewMeetingStore(meetDir).Append(Meeting{Start: at, RecordingSeconds: 60, Text: "a call"}); err != nil {
		t.Fatal(err)
	}

	if _, err := MigrateMeetings(NewStore(dayDir), NewMeetingStore(meetDir), filepath.Join(t.TempDir(), "migrated")); err != nil {
		t.Fatal(err)
	}

	got, _ := NewMeetingStore(meetDir).All()
	if len(got) != 1 {
		t.Fatalf("got %d meetings, want the retry to converge on 1", len(got))
	}
}

// An unparseable file (truncated by a crash, or a stray JSON dropped in the
// transcripts directory) must not block migration for every other file. If
// it did, the sentinel would never land and every launch would retry -- and
// fail -- forever.
func TestMigrationSkipsAnUnparseableFileInsteadOfAborting(t *testing.T) {
	dayDir, meetDir := t.TempDir(), t.TempDir()
	writeRawDayFile(t, dayDir, "2026-08-01.json",
		`[{"timestamp":"2026-08-01T11:00:00Z","kind":"meeting","recording_seconds":60,"text":"a call"}]`)
	writeRawDayFile(t, dayDir, "2026-08-02.json", `{not valid json`)
	sentinel := filepath.Join(t.TempDir(), "migrated")

	n, err := MigrateMeetings(NewStore(dayDir), NewMeetingStore(meetDir), sentinel)
	if err != nil {
		t.Fatalf("migration returned an error instead of skipping the bad file: %v", err)
	}
	if n != 1 {
		t.Fatalf("moved %d meetings, want 1 from the readable file", n)
	}

	got, _ := NewMeetingStore(meetDir).All()
	if len(got) != 1 || got[0].Text != "a call" {
		t.Fatalf("got %+v, want the meeting from the good file migrated", got)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Error("the sentinel was not written -- an unreadable file would block migration forever")
	}
}

func TestMigrationHandlesNoHistoryAtAll(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "migrated")
	n, err := MigrateMeetings(NewStore(t.TempDir()), NewMeetingStore(t.TempDir()), sentinel)
	if err != nil || n != 0 {
		t.Fatalf("moved %d, %v; want 0 and no error on an empty history", n, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Error("the sentinel should still be written, so an empty history is not re-scanned every launch")
	}
}

func TestMigrateDictationsImportsEveryDayFileOnce(t *testing.T) {
	dayDir := t.TempDir()
	writeRawDayFile(t, dayDir, "2026-08-01.json", `[
	  {"timestamp":"2026-08-01T09:00:00Z","duration_seconds":1.5,"text":"first","recording_seconds":3},
	  {"timestamp":"2026-08-01T11:00:00Z","kind":"meeting","recording_seconds":2531,"text":"a call"}
	]`)
	writeRawDayFile(t, dayDir, "2026-08-02.json", `[
	  {"timestamp":"2026-08-02T09:00:00Z","text":"second","audio_path":"/tmp/d.wav","auto_started":true}
	]`)
	writeRawDayFile(t, dayDir, "2026-08-03.json", `[not json`)

	days := NewStore(dayDir)
	sentinel := filepath.Join(t.TempDir(), "migrated-dictations")

	n, err := MigrateDictations(days, sentinel)
	if err != nil {
		t.Fatal(err)
	}
	// Two dictations. The meeting is MigrateMeetings' business, and the
	// unreadable file must not block the sentinel forever.
	if n != 2 {
		t.Fatalf("imported %d, want 2", n)
	}
	got, err := days.AllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" {
		t.Fatalf("got %+v, want first then second", got)
	}
	if got[1].AudioPath != "/tmp/d.wav" || !got[1].AutoStarted {
		t.Fatalf("got %+v, want every field carried across", got[1])
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("the sentinel was not written: %v", err)
	}

	// Re-running is a no-op, and the day files are still there -- they are a
	// frozen backup for one release, not an intermediate to clean up.
	if n, err := MigrateDictations(days, sentinel); err != nil || n != 0 {
		t.Fatalf("second run imported %d (err %v), want 0", n, err)
	}
	if _, err := os.Stat(filepath.Join(dayDir, "2026-08-01.json")); err != nil {
		t.Fatalf("the day file was removed: %v", err)
	}
	if got, _ := days.AllEntries(); len(got) != 2 {
		t.Fatalf("got %d entries after a second run, want 2", len(got))
	}
}

// Deleting the sentinel is how the import is re-run by hand if the database
// ever has to be rebuilt, so it has to be safe to run over rows it already
// wrote.
func TestMigrateDictationsIsSafeToRunAgain(t *testing.T) {
	dayDir := t.TempDir()
	writeRawDayFile(t, dayDir, "2026-08-01.json",
		`[{"timestamp":"2026-08-01T09:00:00Z","text":"once","recording_seconds":3}]`)

	days := NewStore(dayDir)
	for i := 0; i < 2; i++ {
		if _, err := MigrateDictations(days, filepath.Join(t.TempDir(), "migrated")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := days.AllEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "once" {
		t.Fatalf("got %+v, want exactly one entry", got)
	}
}
