package main

import (
	"log"
	"runtime/debug"
	"sort"
	"sync"
)

// decodeQueue runs transcriptions one at a time, off the hotkey path.
//
// Two things force a queue rather than a goroutine per take. One model is
// resident at a time (parakeet's weights alone are 2.4GB), so a second decode
// would either evict it or wait on the same mutex anyway. And the recognizer
// itself is not built to be entered twice at once.
//
// What the queue adds over "wait your turn" is order: the shortest pending
// take goes next, and a long one hands over between blocks (see yield). That
// is what keeps a ten-second remark dictated during a meeting from sitting
// behind the hour of audio the meeting just produced.
type decodeQueue struct {
	mu      sync.Mutex
	pending []*decodeJob
	wake    chan struct{}
	// running is the length of the job the worker is inside, or 0 when idle.
	// Compared against pending jobs to decide who preempts whom.
	running float64
	// onChange reports how many takes are queued or in flight, for the menu
	// bar. Called without the lock held.
	onChange func(int)
	inFlight int
}

// decodeJob is one take's worth of work. run does the actual decoding and is
// handed a yield to call between blocks; everything about models, diarization
// and history lives in the closure, not here.
type decodeJob struct {
	seconds float64
	run     func(yield func())
}

func newDecodeQueue(onChange func(int)) *decodeQueue {
	q := &decodeQueue{wake: make(chan struct{}, 1), onChange: onChange}
	go q.work()
	return q
}

// submit queues a take. seconds is how long the recording is, which is what
// the queue orders by -- not how long decoding will take, which nobody knows
// until it is done, but a good enough proxy: decode time tracks length.
func (q *decodeQueue) submit(seconds float64, run func(yield func())) {
	q.mu.Lock()
	q.pending = append(q.pending, &decodeJob{seconds: seconds, run: run})
	q.inFlight++
	n := q.inFlight
	q.mu.Unlock()

	q.notify(n)
	select {
	case q.wake <- struct{}{}:
	default: // already awake; it will find the job when it looks
	}
}

func (q *decodeQueue) notify(n int) {
	if q.onChange != nil {
		q.onChange(n)
	}
}

// work is the single decoding goroutine.
func (q *decodeQueue) work() {
	for range q.wake {
		for {
			job := q.take(0)
			if job == nil {
				break
			}
			q.execute(job)
		}
	}
}

// take removes the shortest pending job, or nil if there is nothing to do.
// With shorterThan > 0 only a job shorter than that is taken -- which is how
// yield picks out the takes worth interrupting a long decode for.
func (q *decodeQueue) take(shorterThan float64) *decodeJob {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.pending) == 0 {
		return nil
	}
	sort.SliceStable(q.pending, func(i, j int) bool {
		return q.pending[i].seconds < q.pending[j].seconds
	})
	job := q.pending[0]
	if shorterThan > 0 && job.seconds >= shorterThan {
		return nil
	}
	q.pending = q.pending[1:]
	return job
}

// execute runs one job with its own yield, and survives it panicking: a bad
// model or a truncated file takes cgo down with it, and losing the app in the
// middle of a meeting would be far worse than losing one transcript.
func (q *decodeQueue) execute(job *decodeJob) {
	q.mu.Lock()
	outer := q.running
	q.running = job.seconds
	q.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC during transcription: %v\n%s", r, debug.Stack())
			notify("Transcription failed unexpectedly. See ~/Library/Logs/Voxlog.log")
		}
		q.mu.Lock()
		q.running = outer
		q.inFlight--
		n := q.inFlight
		q.mu.Unlock()
		q.notify(n)
	}()

	job.run(func() { q.yield(job.seconds) })
}

// yield is called by a long decode between blocks. Any pending take shorter
// than the one running is decoded right here, before the caller continues --
// the long job is paused rather than preempted, which needs no second worker
// and cannot reorder two halves of the same recording.
//
// Nesting is bounded: each job run from here is strictly shorter than the one
// that yielded, so the chain cannot grow without takes getting shorter each
// time.
func (q *decodeQueue) yield(seconds float64) {
	for {
		job := q.take(seconds)
		if job == nil {
			return
		}
		q.execute(job)
	}
}

// outstanding is how many takes are queued or running. The background
// backfill reads it to stay out of the way: it only starts re-reading an old
// meeting when nothing the user is waiting for is in the queue.
func (q *decodeQueue) outstanding() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inFlight
}
