package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"voxlog-go/internal/history"
)

// The backlog is driven entirely by what is in the database, so these tests
// go straight at that: which meeting gets picked next, and what stops one
// being picked forever.

func backfillStore(t *testing.T) *history.MeetingStore {
	t.Helper()
	return history.NewMeetingStore(t.TempDir())
}

func addMeeting(t *testing.T, s *history.MeetingStore, at time.Time, audio string) {
	t.Helper()
	if err := s.Append(history.Meeting{Start: at, RecordingSeconds: 300, AudioPath: audio}); err != nil {
		t.Fatal(err)
	}
}

func TestNeedsTurnsPicksTheNewestMeetingWithoutThem(t *testing.T) {
	s := backfillStore(t)
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.Local)
	addMeeting(t, s, base, "/tmp/old.wav")
	addMeeting(t, s, base.Add(48*time.Hour), "/tmp/new.wav")

	m, ok, err := s.NeedsTurns(turnsSchemaVersion, backfillAttempts)
	if err != nil || !ok {
		t.Fatalf("got ok=%v, err=%v", ok, err)
	}
	// Newest first: the meeting the user is most likely to open next.
	if m.AudioPath != "/tmp/new.wav" {
		t.Fatalf("picked %s, want the newest meeting", m.AudioPath)
	}
}

func TestAMeetingWithCurrentTurnsIsNeverPickedAgain(t *testing.T) {
	s := backfillStore(t)
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.Local)
	addMeeting(t, s, at, "/tmp/a.wav")

	if err := s.ReplaceTurns(at,
		[]history.MeetingSpeaker{{LocalID: 0, TalkSecs: 10}},
		[]history.Turn{{StartSecs: 0, EndSecs: 10, LocalID: 0, Text: "hello"}},
		turnsSchemaVersion); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.NeedsTurns(turnsSchemaVersion, backfillAttempts); err != nil || ok {
		t.Fatalf("a meeting that already has turns was queued again (ok=%v, err=%v)", ok, err)
	}
	// Until the extraction changes, that is: raising the version is what
	// re-queues everything through an improved pipeline.
	if _, ok, err := s.NeedsTurns(turnsSchemaVersion+1, backfillAttempts); err != nil || !ok {
		t.Fatalf("raising the version did not re-queue the meeting (ok=%v, err=%v)", ok, err)
	}
}

// A decode that dies takes nothing with it, so the meeting comes back around
// -- but only so many times, or one bad recording loops forever.
func TestAttemptsAreCappedSoOneBadRecordingCannotLoop(t *testing.T) {
	s := backfillStore(t)
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.Local)
	addMeeting(t, s, at, "/tmp/a.wav")

	for i := 0; i < backfillAttempts; i++ {
		if _, ok, _ := s.NeedsTurns(turnsSchemaVersion, backfillAttempts); !ok {
			t.Fatalf("gave up after %d attempt(s), want %d", i, backfillAttempts)
		}
		if err := s.NoteBackfillAttempt(at); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := s.NeedsTurns(turnsSchemaVersion, backfillAttempts); ok {
		t.Fatal("a meeting that failed three times is still being retried")
	}
}

// Retention having swept the audio is not a failure to retry: nothing will be
// different next time, and re-checking it forever hides the meetings that can
// still be recovered.
func TestGivingUpOnSweptAudio(t *testing.T) {
	s := backfillStore(t)
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.Local)
	addMeeting(t, s, at, filepath.Join(t.TempDir(), "gone.wav"))

	m, ok, err := s.NeedsTurns(turnsSchemaVersion, backfillAttempts)
	if err != nil || !ok {
		t.Fatalf("got ok=%v, err=%v", ok, err)
	}
	if _, err := os.Stat(m.AudioPath); err == nil {
		t.Fatal("the test's own audio path exists; it must not")
	}
	if err := s.GiveUpOnTurns(at, turnsSchemaVersion, "the recording is no longer on disk"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.NeedsTurns(turnsSchemaVersion, backfillAttempts); ok {
		t.Fatal("a meeting with no audio left is still queued")
	}
}

// A meeting that was never transcribed and has no audio either cannot be
// recovered from anything, and must not sit at the head of the queue.
func TestAMeetingWithNoAudioPathIsNeverQueued(t *testing.T) {
	s := backfillStore(t)
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.Local)
	if err := s.Append(history.Meeting{Start: at, RecordingSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.NeedsTurns(turnsSchemaVersion, backfillAttempts); ok {
		t.Fatal("a meeting with no recording was queued for re-reading")
	}
}

// startDictation and startMeeting both run with a.mu already held -- their
// own comments say so -- and a.mu is a plain sync.Mutex. Anything they call
// that takes it again wedges the hotkey path completely: every press blocks
// forever, holding the lock, and no log line is ever written because the
// deadlock happens before the first one.
//
// That shipped once. This is the guard.
func TestStoppingTheBacklogDoesNotTakeTheAppLock(t *testing.T) {
	a := &app{}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.backfillStop = make(chan struct{})

	done := make(chan struct{})
	go func() {
		a.stopBackfillLocked()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the backlog blocked on a.mu -- the hotkey path deadlocks like this")
	}
	if a.backfillStop != nil {
		t.Fatal("the stop channel was not cleared")
	}
}
