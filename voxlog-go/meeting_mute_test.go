package main

import (
	"testing"
	"time"
)

// Mute is per recording, not a setting: the next meeting starts unmuted no
// matter how the last one ended, and the menu item's label follows the state
// it actually landed in.
func TestMeetingMuteTogglesAndDiesWithTheMeeting(t *testing.T) {
	a := &app{meeting: &meeting{start: time.Now()}}

	if !a.toggleMeetingMute() {
		t.Fatal("first toggle did not mute")
	}
	if a.toggleMeetingMute() {
		t.Fatal("second toggle did not unmute")
	}

	a.meeting.muted = true
	a.meeting = nil // the meeting ended
	if a.toggleMeetingMute() {
		t.Error("muted with no meeting running; the button should be inert")
	}
	if got := muteMicLabel(false); got == muteMicLabel(true) {
		t.Error("the mute item reads the same either way")
	}
}

// The menu draws its own dot beside the clock, so the clock must not bring
// one of its own -- the menu bar title still does.
func TestClockLabel(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0:00"},
		{-time.Second, "0:00"},
		{95 * time.Second, "1:35"},
		{3*time.Hour + 4*time.Minute + 5*time.Second, "3:04:05"},
	} {
		if got := clockLabel(c.d); got != c.want {
			t.Errorf("clockLabel(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	if got := elapsedLabel(95 * time.Second); got != "● 1:35" {
		t.Errorf("elapsedLabel = %q, want the menu bar's dot in front", got)
	}
}

// The menu is redrawn from a two-second poll, not only when a recording
// starts, so the title it draws has to come from the meeting's own state. It
// used to be hardcoded to "Mute my microphone", which took the user's mute
// back off the menu within one tick of them clicking it.
func TestTheMuteItemsTitleFollowsTheRunningMeeting(t *testing.T) {
	a := &app{meeting: &meeting{start: time.Now()}}

	if got := muteMicLabel(a.meetingMuted()); got != muteMicLabel(false) {
		t.Errorf("a fresh recording reads %q, want the unmuted title", got)
	}
	a.toggleMeetingMute()
	// Every redraw from here on, poll or otherwise, asks the same question.
	for i := 0; i < 3; i++ {
		if got := muteMicLabel(a.meetingMuted()); got != muteMicLabel(true) {
			t.Fatalf("redraw %d reads %q, want the muted title", i, got)
		}
	}
}
