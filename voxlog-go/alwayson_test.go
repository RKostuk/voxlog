package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"voxlog-go/internal/history"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/vad"
)

func TestExcludedMatchesByNameCaseInsensitively(t *testing.T) {
	list := []string{"1Password", " Zoom "}
	for _, app := range []string{"1Password", "1password", "Zoom"} {
		if !excluded(app, list) {
			t.Errorf("excluded(%q) = false, want true", app)
		}
	}
	for _, app := range []string{"", "Safari", "Zoom Rooms"} {
		if excluded(app, list) {
			t.Errorf("excluded(%q) = true, want false", app)
		}
	}
}

func TestExcludedWithNoListNeverExcludes(t *testing.T) {
	if excluded("Safari", nil) {
		t.Fatal("an empty exclusion list must not exclude anything")
	}
}

func TestPauseStopsListeningUntilResumed(t *testing.T) {
	a := &app{}
	if a.listen.isPaused() {
		t.Fatal("listening starts unpaused")
	}
	if !a.toggleListenPause() {
		t.Fatal("first toggle should pause")
	}
	if !a.listen.isPaused() {
		t.Fatal("pause did not stick")
	}
	if a.toggleListenPause() {
		t.Fatal("second toggle should resume")
	}
	// The label says what the click will do, not what the state is.
	if listenPauseLabel(true) != "Resume listening" || listenPauseLabel(false) != "Pause listening" {
		t.Fatal("the menu label is backwards")
	}
}

func TestPendingIsConsumedOnce(t *testing.T) {
	// The audio thread raises the flag and the supervisor acts on it. Reading
	// it twice would start two recordings for one stretch of speech.
	var l alwaysOn
	l.pending = true
	if !l.takePending() {
		t.Fatal("takePending did not see the flag")
	}
	if l.takePending() {
		t.Fatal("takePending returned the same flag twice")
	}
}

// A listener with no gate is the state between "the setting was turned on"
// and "the model finished downloading". Audio still arrives; nothing may be
// kept, and nothing may crash.
func TestChunksWithoutAGateAreDropped(t *testing.T) {
	var l alwaysOn
	l.onChunk(make([]float32, 1000), func(vad.Segment) bool {
		t.Fatal("a listener with no gate must not confirm anything")
		return false
	})
	if len(l.preroll) != 0 {
		t.Fatalf("kept %d samples with no gate open", len(l.preroll))
	}
}

func TestAutoRecordingsWithoutATranscriptAreSwept(t *testing.T) {
	dir := t.TempDir()
	store := settings.NewStore(filepath.Join(dir, "settings.json"))
	cfg := store.Get()
	cfg.AlwaysOn = true
	cfg.AlwaysOnRetentionHours = 1
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}

	meetings := history.NewMeetingStore(dir)
	old := time.Now().Add(-3 * time.Hour)
	recent := time.Now().Add(-10 * time.Minute)

	write := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("not really audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	audioPath, keptPath, manualPath := write("old.wav"), write("kept.wav"), write("manual.wav")

	for _, m := range []history.Meeting{
		{Start: old, AudioPath: audioPath, AutoStarted: true},
		{Start: recent, AudioPath: keptPath, AutoStarted: true},
		{Start: old.Add(-time.Minute), AudioPath: manualPath},
	} {
		if err := meetings.Append(m); err != nil {
			t.Fatal(err)
		}
	}

	a := &app{store: store, meetings: meetings}
	a.sweepAutoRecordings()

	all, err := meetings.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		switch {
		case m.Start.Equal(old):
			if m.AudioPath != "" {
				t.Error("an old auto recording with no transcript kept its audio")
			}
		case m.Start.Equal(recent):
			if m.AudioPath == "" {
				t.Error("a recent auto recording was swept too early")
			}
		default:
			if m.AudioPath == "" {
				t.Error("a recording the user started by hand was swept")
			}
		}
	}
}
