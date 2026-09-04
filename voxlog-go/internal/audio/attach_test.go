package audio

import (
	"math"
	"sync"
	"testing"
)

func TestAttachSeesEveryChunk(t *testing.T) {
	r := &Recorder{gain: 1.0}
	var got []float32
	detach := r.Attach(func(c []float32) { got = append(got, c...) })
	defer detach()

	r.onData(nil, pcmBytes([]float32{0.1, 0.2}), 2)
	r.onData(nil, pcmBytes([]float32{0.3}), 1)

	want := []float32{0.1, 0.2, 0.3}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Fatalf("sample %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestAttachDeliversGainedSamples(t *testing.T) {
	// A dictation listening in on a meeting's capture must hear exactly what
	// the meeting's own buffer holds -- otherwise the same words would be
	// recognized differently depending on which recording they came out of.
	r := &Recorder{gain: 2.0}
	var got []float32
	r.Attach(func(c []float32) { got = append(got, c...) })
	r.onData(nil, pcmBytes([]float32{0.25}), 1)

	if len(got) != 1 || math.Abs(float64(got[0]-0.5)) > 1e-6 {
		t.Fatalf("got %v, want the gained sample 0.5", got)
	}
}

func TestDetachStopsDelivery(t *testing.T) {
	r := &Recorder{gain: 1.0}
	count := 0
	detach := r.Attach(func([]float32) { count++ })

	r.onData(nil, pcmBytes([]float32{0.1}), 1)
	detach()
	r.onData(nil, pcmBytes([]float32{0.1}), 1)
	detach() // idempotent: a second detach must not panic

	if count != 1 {
		t.Fatalf("listener called %d times, want 1 -- detach did not take effect", count)
	}
}

func TestAttachDoesNotDisturbTheRecordersOwnBuffer(t *testing.T) {
	r := &Recorder{gain: 1.0}
	r.Attach(func([]float32) {})
	r.onData(nil, pcmBytes([]float32{0.1, 0.2}), 2)

	if got := r.Stop(); len(got) != 2 {
		t.Fatalf("recorder buffered %v, want both samples", got)
	}
}

func TestAttachWhileCaptureRuns(t *testing.T) {
	// Attaching happens on the dictation goroutine while chunks are arriving
	// on miniaudio's thread; the race detector is the point of this test.
	r := &Recorder{gain: 1.0}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.onData(nil, pcmBytes([]float32{0.1}), 1)
			}
		}
	}()

	for i := 0; i < 50; i++ {
		detach := r.Attach(func([]float32) {})
		detach()
	}
	close(stop)
	wg.Wait()
}

func TestAttachNilIsANoOp(t *testing.T) {
	r := &Recorder{gain: 1.0}
	detach := r.Attach(nil)
	r.onData(nil, pcmBytes([]float32{0.1}), 1)
	detach()
}
