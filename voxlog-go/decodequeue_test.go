package main

import (
	"sync"
	"testing"
	"time"
)

// waitFor blocks until cond holds or the test gives up, so the tests do not
// depend on how fast the worker goroutine happens to be scheduled.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueRunsSubmittedJobs(t *testing.T) {
	q := newDecodeQueue(nil)
	done := make(chan struct{})
	q.submit("test", "", 3, func(func()) { close(done) })

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job never ran")
	}
}

func TestQueueRunsOneJobAtATime(t *testing.T) {
	// One resident model, one recognizer: two decodes overlapping would be
	// either an eviction storm or a crash.
	q := newDecodeQueue(nil)
	var mu sync.Mutex
	concurrent, peak, finished := 0, 0, 0

	for i := 0; i < 5; i++ {
		q.submit("test", "", float64(i+1), func(func()) {
			mu.Lock()
			concurrent++
			if concurrent > peak {
				peak = concurrent
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			concurrent--
			finished++
			mu.Unlock()
		})
	}

	waitFor(t, "all jobs", func() bool { mu.Lock(); defer mu.Unlock(); return finished == 5 })
	if peak != 1 {
		t.Fatalf("%d decodes ran at once, want 1", peak)
	}
}

func TestShortestQueuedTakeGoesFirst(t *testing.T) {
	q := newDecodeQueue(nil)
	var mu sync.Mutex
	var order []float64

	// Block the worker so everything else queues up behind one job.
	release := make(chan struct{})
	q.submit("test", "", 1, func(func()) { <-release })
	waitFor(t, "the worker to pick up the blocker", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.running == 1
	})

	for _, seconds := range []float64{3600, 12, 300} {
		s := seconds
		q.submit("test", "", s, func(func()) {
			mu.Lock()
			order = append(order, s)
			mu.Unlock()
		})
	}
	close(release)

	waitFor(t, "the queue to drain", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 3 })
	want := []float64{12, 300, 3600}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("ran in order %v, want shortest first %v", order, want)
		}
	}
}

func TestLongJobYieldsToAShorterOne(t *testing.T) {
	// The point of the whole queue: a dictation recorded during a meeting is
	// decoded between the meeting's blocks, not after all of them.
	q := newDecodeQueue(nil)
	var mu sync.Mutex
	var events []string

	queued := make(chan struct{})
	meetingDone := make(chan struct{})

	q.submit("test", "", 3600, func(yield func()) {
		<-queued // the dictation is now waiting
		for block := 0; block < 3; block++ {
			mu.Lock()
			events = append(events, "meeting-block")
			mu.Unlock()
			yield()
		}
		close(meetingDone)
	})
	waitFor(t, "the meeting decode to start", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.running == 3600
	})

	q.submit("test", "", 10, func(func()) {
		mu.Lock()
		events = append(events, "dictation")
		mu.Unlock()
	})
	close(queued)

	select {
	case <-meetingDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the meeting decode never finished")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("got %v, want three blocks and the dictation", events)
	}
	if events[len(events)-1] == "dictation" {
		t.Fatalf("got %v, want the dictation decoded between blocks, not after them", events)
	}
}

func TestYieldIgnoresJobsThatAreNotShorter(t *testing.T) {
	// Yielding to something just as long buys nothing and would let two long
	// recordings interleave for no reason.
	q := newDecodeQueue(nil)
	var mu sync.Mutex
	var events []string

	queued := make(chan struct{})
	first := make(chan struct{})

	q.submit("test", "", 600, func(yield func()) {
		<-queued
		yield()
		mu.Lock()
		events = append(events, "first-done")
		mu.Unlock()
		close(first)
	})
	waitFor(t, "the first job to start", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.running == 600
	})

	q.submit("test", "", 900, func(func()) {
		mu.Lock()
		events = append(events, "second")
		mu.Unlock()
	})
	close(queued)

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("the first job never finished")
	}
	waitFor(t, "the second job", func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 2 })

	mu.Lock()
	defer mu.Unlock()
	if events[0] != "first-done" {
		t.Fatalf("got %v, want the longer job to wait its turn", events)
	}
}

func TestQueueReportsHowMuchIsOutstanding(t *testing.T) {
	// This is what the menu bar reads (via len): it must reach zero, or the
	// icon would claim Voxlog is decoding forever.
	var mu sync.Mutex
	var reports [][]decodeStatus
	q := newDecodeQueue(func(list []decodeStatus) {
		mu.Lock()
		reports = append(reports, list)
		mu.Unlock()
	})

	q.submit("first", "", 1, func(func()) {})
	q.submit("second", "", 2, func(func()) {})

	waitFor(t, "the queue to report empty", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reports) > 0 && len(reports[len(reports)-1]) == 0
	})
}

func TestQueueReportsWhatIsDecoding(t *testing.T) {
	// The main window draws these labels, so a queued take has to be
	// identifiable as a specific recording, not just "one of two".
	release := make(chan struct{})
	seen := make(chan []decodeStatus, 32)
	q := newDecodeQueue(func(list []decodeStatus) { seen <- list })

	// The meeting has to be picked up before the dictation is submitted, or
	// shortest-first would hand the worker the dictation instead -- which is
	// correct behaviour, just not the arrangement under test here.
	q.submit("Meeting, 09:00", "", 600, func(func()) { <-release })
	waitFor(t, "the meeting to start decoding", func() bool {
		select {
		case list := <-seen:
			return len(list) == 1 && list[0].Running
		default:
		}
		return false
	})
	q.submit("Dictation, 14:32", "", 5, func(func()) {})

	// The long one keeps decoding while the short one waits behind it: the
	// job above offers no yield, so ordering cannot rescue it here.
	var got []decodeStatus
	waitFor(t, "both takes to be reported", func() bool {
		select {
		case list := <-seen:
			if len(list) == 2 {
				got = list
				return true
			}
		default:
		}
		return false
	})
	close(release)

	if got[0].Label != "Meeting, 09:00" || !got[0].Running {
		t.Fatalf("first entry = %+v, want the meeting, running", got[0])
	}
	if got[1].Label != "Dictation, 14:32" || got[1].Running {
		t.Fatalf("second entry = %+v, want the dictation, waiting", got[1])
	}
	if got[1].Seconds != 5 {
		t.Fatalf("seconds = %v, want the recording length 5", got[1].Seconds)
	}
}

func TestAPanickingDecodeDoesNotTakeTheQueueDown(t *testing.T) {
	// sherpa-onnx aborts on a truncated model file. Losing one transcript is
	// acceptable; losing the app mid-meeting is not.
	q := newDecodeQueue(nil)
	q.submit("test", "", 1, func(func()) { panic("bad model") })

	done := make(chan struct{})
	q.submit("test", "", 2, func(func()) { close(done) })

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the queue died with the panicking job")
	}
}
