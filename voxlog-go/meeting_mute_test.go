package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voxlog-go/internal/settings"
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

// The recording notice is a courtesy the user asked for -- and in some places
// the law. It goes out once, when the recording starts, and not at all if
// they turned it off.
func TestTheRecordingNoticeFollowsItsSetting(t *testing.T) {
	var posted []string
	real := postRecordingNotice
	postRecordingNotice = func(msg string) { posted = append(posted, msg) }
	t.Cleanup(func() { postRecordingNotice = real })

	store := settings.NewStore(filepath.Join(t.TempDir(), "settings.json"))
	a := &app{store: store}

	cfg := store.Get()
	cfg.RecordingNotice = true
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	a.warnAboutRecording()
	if len(posted) != 1 {
		t.Fatalf("notice posted %d times with the setting on, want 1", len(posted))
	}
	if !strings.Contains(posted[0], "let the others on the call know") {
		t.Errorf("the notice reads %q", posted[0])
	}

	cfg.RecordingNotice = false
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	a.warnAboutRecording()
	if len(posted) != 1 {
		t.Fatalf("the notice went out %d times with the setting off", len(posted)-1)
	}
}

// The menu bar and the window's banner are both redrawn from inside
// startMeeting and stopMeeting, which hold a.mu for the whole of their work.
// Anything they read that needs a.mu deadlocks the app against itself on the
// first meeting: the recording runs, and the Stop item, the meeting hotkey
// and the Overview poll all wait on a lock nobody will release.
//
// The fields are set directly rather than through startedMeeting/setMicMuted
// because those repaint the menu bar icon, which means systray and a status
// item this test has neither of.
func TestMenuStateIsReadableWhileTheAppLockIsHeld(t *testing.T) {
	tr := &tray{meeting: true, meetingSince: time.Now(), micMuted: true}
	a := &app{tray: tr}

	a.mu.Lock()
	defer a.mu.Unlock()

	done := make(chan struct{})
	var muted, running bool
	var since time.Time
	go func() {
		defer close(done)
		muted = a.tray.meetingMicMuted()
		running, since, _ = a.tray.meetingSnapshot()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reading the menu's own state blocked on a.mu")
	}
	if !muted {
		t.Error("the menu would draw an unmuted microphone for a muted recording")
	}
	if !running || since.IsZero() {
		t.Errorf("snapshot says running=%v since=%v", running, since)
	}
}

// Always-on's mute counts in the same snapshot: to the user there is one
// fact, "the app is recording and cannot hear me".
func TestTheSnapshotCoversEitherMute(t *testing.T) {
	tr := &tray{meeting: true, meetingSince: time.Now(), listenMuted: true}
	if _, _, muted := tr.meetingSnapshot(); !muted {
		t.Error("always-on's mute did not reach the banner")
	}
	stopped := &tray{}
	if running, _, muted := stopped.meetingSnapshot(); running || muted {
		t.Errorf("with nothing recording: running=%v muted=%v", running, muted)
	}
}
