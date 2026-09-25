package usernotify

import (
	"sync"
	"testing"
	"time"
)

// reset puts the package back to "the app just launched, nobody has answered
// the permission prompt" between tests.
func reset(t *testing.T, answerComing bool) {
	t.Helper()
	real := willDecide
	mu.Lock()
	decided, granted, fellBack = false, false, false
	pending = nil
	if pendingWait != nil {
		pendingWait.Stop()
		pendingWait = nil
	}
	mu.Unlock()
	willDecide = func() bool { return answerComing }
	t.Cleanup(func() {
		willDecide = real
		mu.Lock()
		decided, granted, fellBack = false, false, false
		pending = nil
		if pendingWait != nil {
			pendingWait.Stop()
			pendingWait = nil
		}
		mu.Unlock()
	})
}

// While the permission prompt is on screen, a banner waits for it. It used to
// be flushed to osascript after three seconds -- a banner with the wrong icon
// whose click opens Finder, which is exactly what "the other notifications
// arrive differently and I cannot get into the app" was.
func TestABannerWaitsForAnAnswerThatIsComing(t *testing.T) {
	reset(t, true)

	Post("Meeting transcript ready.", "meetings")

	mu.Lock()
	queued, timer := len(pending), pendingWait
	mu.Unlock()
	if queued != 1 {
		t.Fatalf("%d banners queued, want 1", queued)
	}
	if timer != nil {
		t.Error("a fallback timer was armed while an answer was still coming")
	}
}

// ...and with nothing to wait for -- an unbundled build, where the completion
// handler will never run -- the queue must not be held forever.
func TestABannerWithNoAnswerComingGetsATimer(t *testing.T) {
	reset(t, false)

	Post("Transcription failed.", "history")

	mu.Lock()
	timer := pendingWait
	mu.Unlock()
	if timer == nil {
		t.Fatal("nothing would ever release this banner")
	}
}

// The answer arriving is what delivers what was queued, in the order it was
// posted, through the same path a later banner would take.
func TestTheAnswerDrainsTheQueue(t *testing.T) {
	reset(t, true)

	var mu2 sync.Mutex
	var sent []pendingPost
	realPost := postNow
	postNow = func(message, action string) {
		mu2.Lock()
		sent = append(sent, pendingPost{message, action})
		mu2.Unlock()
	}
	t.Cleanup(func() { postNow = realPost })

	Post("first", "history")
	Post("second", "meetings")
	goNotifyAuthDecided(1)

	mu2.Lock()
	defer mu2.Unlock()
	if len(sent) != 2 {
		t.Fatalf("delivered %d banners, want 2: %+v", len(sent), sent)
	}
	if sent[0].message != "first" || sent[1].message != "second" {
		t.Errorf("delivered out of order: %+v", sent)
	}
	if sent[0].action != "history" || sent[1].action != "meetings" {
		t.Errorf("the destinations did not survive the queue: %+v", sent)
	}
	if st := Status(); !st.Decided || !st.Granted {
		t.Errorf("Status() = %+v after a granted answer", st)
	}
}

// Once the answer is in, a banner goes straight out rather than queueing.
func TestPostAfterTheAnswerDoesNotQueue(t *testing.T) {
	reset(t, true)
	realPost := postNow
	var got pendingPost
	postNow = func(message, action string) { got = pendingPost{message, action} }
	t.Cleanup(func() { postNow = realPost })

	goNotifyAuthDecided(1)
	Post("Reminder: water the plants", "tasks")

	if got.message == "" {
		t.Fatal("nothing was delivered")
	}
	if got.action != "tasks" {
		t.Errorf("action %q, want tasks", got.action)
	}
	mu.Lock()
	queued := len(pending)
	mu.Unlock()
	if queued != 0 {
		t.Errorf("%d banners queued after the answer was in", queued)
	}
}

// The timed-out flush is the unbundled path; it must still deliver.
func TestTheTimeoutStillDelivers(t *testing.T) {
	reset(t, false)
	realPost := postNow
	done := make(chan pendingPost, 1)
	postNow = func(message, action string) { done <- pendingPost{message, action} }
	t.Cleanup(func() { postNow = realPost })

	Post("No model selected. Pick one in Settings.", "settings")
	flushPendingTimedOut()

	select {
	case p := <-done:
		if p.action != "settings" {
			t.Errorf("action %q, want settings", p.action)
		}
	case <-time.After(time.Second):
		t.Fatal("the flush delivered nothing")
	}
}
