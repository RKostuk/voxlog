package main

import (
	"testing"
	"time"

	"voxlog-go/internal/history"
)

// TestASecondTakeIsNotHeldUpByTheFirstsTranscript is the complaint this whole
// change exists for: the dictate key used to do nothing until the previous
// take had finished decoding.
func TestASecondTakeIsNotHeldUpByTheFirstsTranscript(t *testing.T) {
	hist := history.NewStore(t.TempDir())
	q := newDecodeQueue(nil)

	// The first take's decode is still running...
	firstDecoding := make(chan struct{})
	releaseFirst := make(chan struct{})
	q.submit("test", "", 8, func(func()) {
		close(firstDecoding)
		<-releaseFirst
		if err := hist.Append(history.Entry{Timestamp: time.Now(), Text: "first"}); err != nil {
			t.Errorf("append: %v", err)
		}
	})
	<-firstDecoding

	// ...and the second take records and submits regardless. Submitting is
	// what the hotkey path actually does; if it blocked here, the key would
	// be dead for as long as the first decode lasts.
	submitted := make(chan struct{})
	go func() {
		q.submit("test", "", 5, func(func()) {
			if err := hist.Append(history.Entry{Timestamp: time.Now(), Text: "second"}); err != nil {
				t.Errorf("append: %v", err)
			}
		})
		close(submitted)
	}()

	select {
	case <-submitted:
	case <-time.After(time.Second):
		t.Fatal("submitting a take blocked while an earlier one was decoding")
	}

	close(releaseFirst)
	waitFor(t, "both transcripts", func() bool {
		entries, err := hist.AllEntries()
		return err == nil && len(entries) == 2
	})

	entries, err := hist.AllEntries()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Text] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("history holds %v, want both takes", entries)
	}
}

func TestAFailedDecodeDoesNotStopTheOthers(t *testing.T) {
	hist := history.NewStore(t.TempDir())
	q := newDecodeQueue(nil)

	q.submit("test", "", 3, func(func()) { panic("model went missing") })
	q.submit("test", "", 4, func(func()) {
		if err := hist.Append(history.Entry{Timestamp: time.Now(), Text: "survivor"}); err != nil {
			t.Errorf("append: %v", err)
		}
	})

	waitFor(t, "the surviving transcript", func() bool {
		entries, err := hist.AllEntries()
		return err == nil && len(entries) == 1
	})
}
