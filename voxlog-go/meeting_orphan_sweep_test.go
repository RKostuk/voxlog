package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"voxlog-go/internal/audio"
	"voxlog-go/internal/history"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/task"
)

// writeOrphanMicWAV drops a mic-only recording on disk with no history entry
// -- the shape a crash or a quit mid-call leaves behind -- and backdates it
// so an age-based sweep would otherwise be free to remove it.
func writeOrphanMicWAV(t *testing.T, dir string, age time.Duration) string {
	t.Helper()
	name := time.Now().Add(-age).Format("2006-01-02-150405") + "-mic.wav"
	path := filepath.Join(dir, name)
	w, err := audio.NewWAVWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second of silence is well past minVoicedSeconds, so adoption treats
	// it as a real (if silent) call rather than a false start.
	if err := w.Write(make([]float32, audio.SampleRate)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOrphanedMeetingSurvivesStartupSweep is the scenario the whole-branch
// review caught: a crash leaves a *-mic.wav with no history entry, retention
// is on, and the file is old enough that the sweep would delete it outright.
// recordingsInUse only ever tracks the *current* live meeting, so an orphan
// is invisible to it -- the only thing that can save the meeting's record is
// adoption running before the sweep gets a chance to remove the evidence.
func TestOrphanedMeetingSurvivesStartupSweep(t *testing.T) {
	histDir := t.TempDir()
	hist := history.NewStore(histDir)
	meetings := history.NewMeetingStore(histDir)
	recDir := filepath.Join(histDir, meetingsDirName)
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		t.Fatal(err)
	}

	micPath := writeOrphanMicWAV(t, recDir, 10*24*time.Hour) // past the week cutoff

	store := settings.NewStore(filepath.Join(t.TempDir(), "settings.json"))
	cfg := store.Get()
	cfg.AudioRetention = history.RetentionWeek
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}

	tasks := task.NewStore(t.TempDir())
	a := newApp(store, hist, meetings, tasks, "", nil)

	// This is the order onReady now runs in: adoption first, so the orphan
	// gets a history entry before the sweep can touch its file.
	a.adoptOrphanedMeetings()
	a.sweepRecordings()

	got, err := a.meetings.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AudioPath != micPath {
		t.Fatalf("orphaned meeting was lost: got %+v", got)
	}

	// The retention policy still applies to the audio itself -- adoption
	// only guarantees the meeting is remembered, not that its recording is
	// kept forever.
	if _, err := os.Stat(micPath); !os.IsNotExist(err) {
		t.Fatalf("expected the aged recording to be swept, stat err=%v", err)
	}

	// Reversing the order is exactly the bug: the sweep deletes the orphan's
	// only trace before adoption ever looks at it, and the meeting -- not
	// just its audio -- disappears without a record.
	micPath2 := writeOrphanMicWAV(t, recDir, 10*24*time.Hour)
	a.sweepRecordings()
	a.adoptOrphanedMeetings()

	if _, err := os.Stat(micPath2); !os.IsNotExist(err) {
		t.Fatalf("expected the buggy order to still sweep the file, stat err=%v", err)
	}
	got, err = a.meetings.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("buggy order should have lost the second meeting entirely, got %+v", got)
	}
}
