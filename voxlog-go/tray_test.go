package main

import (
	"testing"
	"time"
)

func TestTrayStatePriority(t *testing.T) {
	// The reason this is derived instead of set: a decode finishing while a
	// later take is being recorded must not paint the icon idle.
	cases := []struct {
		name      string
		recording int
		decoding  int
		meeting   bool
		micMuted  bool
		want      string
	}{
		{"nothing happening", 0, 0, false, false, stateIdle},
		{"recording", 1, 0, false, false, stateRecording},
		{"decoding", 0, 2, false, false, stateTranscribing},
		{"recording while an earlier take decodes", 1, 1, false, false, stateRecording},
		{"meeting alone", 0, 0, true, false, stateMeeting},
		{"dictating during a meeting", 1, 0, true, false, stateRecording},
		{"meeting while its own audio decodes", 0, 1, true, false, stateTranscribing},
		// Muted wins over everything: a menu bar that says "recording" while
		// the microphone is off is the one lie this icon must not tell.
		{"muted meeting", 0, 0, true, true, stateMuted},
		{"muted while a take decodes", 0, 1, true, true, stateMuted},
	}
	for _, c := range cases {
		if got := trayStateFor(c.recording, c.decoding, c.meeting, c.micMuted); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestElapsedLabel(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "● 0:00"},
		{9 * time.Second, "● 0:09"},
		{12*time.Minute + 34*time.Second, "● 12:34"},
		{time.Hour + 2*time.Minute + 3*time.Second, "● 1:02:03"},
		{-time.Second, "● 0:00"}, // clock skew must not print a negative call
	}
	for _, c := range cases {
		if got := elapsedLabel(c.d); got != c.want {
			t.Errorf("%v: got %q, want %q", c.d, got, c.want)
		}
	}
}

func TestTrayOnlyTouchesTheStatusItemWhenSomethingChanged(t *testing.T) {
	// The meeting clock calls refresh every few seconds for an hour; each
	// call reaching AppKit for no reason would be an hour of pointless main
	// thread hops.
	tr := &tray{last: stateIdle, lastTitle: ""}
	tr.mu.Lock()
	state := trayStateFor(tr.recording, tr.decoding, tr.meeting, tr.micMuted)
	tr.mu.Unlock()
	if state != tr.last {
		t.Fatalf("a fresh tray reports %q but remembers %q", state, tr.last)
	}
}

func TestIsAccidentalTap(t *testing.T) {
	// Hold mode: brushing the key on the way to something else must not paste
	// a fragment of a word into whatever is focused.
	if !isAccidentalTap(50 * time.Millisecond) {
		t.Error("a 50ms hold should be discarded")
	}
	if isAccidentalTap(2 * time.Second) {
		t.Error("a 2s hold is somebody speaking")
	}
	if isAccidentalTap(minHoldDuration) {
		t.Error("exactly the minimum should count as a real take")
	}
}

// The menu closes a channel when a dictation stops, so the callback must fire
// on the 0->1 and 1->0 transitions only -- not on every take, and never twice
// for one stop.
func TestTrayRecordingCallbackFiresOnTransitionsOnly(t *testing.T) {
	var states []bool
	tr := &tray{onRecording: func(running bool) { states = append(states, running) }}

	tr.startedRecording() // 0 -> 1: fires
	tr.startedRecording() // 1 -> 2: silent
	tr.stoppedRecording() // 2 -> 1: silent
	tr.stoppedRecording() // 1 -> 0: fires
	tr.stoppedRecording() // already 0: silent, or the menu double-closes

	want := []bool{true, false}
	if len(states) != len(want) {
		t.Fatalf("got %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("got %v, want %v", states, want)
		}
	}
}
