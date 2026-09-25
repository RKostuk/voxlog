package main

import (
	"log"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"voxlog-go/internal/ui"
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
	// onChange reports what is queued or in flight -- one entry per take,
	// in the order they will be decoded. The menu bar only needs len(); the
	// main window draws the labels. Called without the lock held.
	onChange func([]decodeStatus)
	inFlight int
	// active is the stack of jobs currently being executed. It is a stack,
	// not a single job, because yield runs a short take inside a long one:
	// the last entry is the one actually decoding, the ones under it are
	// paused mid-recording and will resume.
	active []*decodeJob
}

// decodeStatus is one line of "what is Voxlog chewing on". Running marks the
// take actually being decoded right now; a job paused by yield reports
// Running false, same as one that has not started, because from the outside
// there is no difference -- neither is producing a transcript yet.
type decodeStatus struct {
	Label string `json:"label"`
	// Kind separates transcription from the LLM work that follows it, so one
	// list can show everything the app is chewing on without the two reading
	// as the same job.
	Kind string `json:"kind"`
	// Stage is what an LLM job is doing right now ("Summarising", "Finding
	// tasks"). Empty for transcription, whose stage is its label.
	Stage string `json:"stage,omitempty"`
	// Key identifies the row this take belongs to, so a list can mark it
	// "Transcribing...". It is the meeting's start in RFC3339Nano -- the same
	// id meetingsJSON gives the row. Empty for a dictation: its history entry
	// does not exist until the decode finishes, so there is no row to mark.
	Key     string  `json:"key"`
	Seconds float64 `json:"seconds"`
	Running bool    `json:"running"`
	// QueuedAtMS is when the job was submitted, in Unix milliseconds, so the
	// page can say how long something has been waiting without a second
	// clock of its own.
	QueuedAtMS int64 `json:"queued_at_ms"`
}

// The two kinds of work the queue view shows. Transcription is this queue's
// own; the LLM jobs are goroutines elsewhere that report into the same list
// (see app.trackLLM) so "what is the machine busy with" has one answer.
const (
	kindTranscribe = "transcribe"
	kindLLM        = "llm"
)

// decodeJob is one take's worth of work. run does the actual decoding and is
// handed a yield to call between blocks; everything about models, diarization
// and history lives in the closure, not here.
type decodeJob struct {
	// label is what the UI calls this take ("Dictation, 14:32", "Meeting,
	// 09:00"). Nothing here parses it; it exists so a queue entry can be
	// matched to the row it came from by eye.
	label    string
	key      string
	queuedAt time.Time
	seconds  float64
	run      func(yield func())
}

func newDecodeQueue(onChange func([]decodeStatus)) *decodeQueue {
	q := &decodeQueue{wake: make(chan struct{}, 1), onChange: onChange}
	go q.work()
	return q
}

// submit queues a take. seconds is how long the recording is, which is what
// the queue orders by -- not how long decoding will take, which nobody knows
// until it is done, but a good enough proxy: decode time tracks length.
func (q *decodeQueue) submit(label, key string, seconds float64, run func(yield func())) {
	q.mu.Lock()
	q.pending = append(q.pending, &decodeJob{
		label:    label,
		key:      key,
		queuedAt: time.Now(),
		seconds:  seconds,
		run:      run,
	})
	q.inFlight++
	statuses := q.statusesLocked()
	q.mu.Unlock()

	q.notify(statuses)
	select {
	case q.wake <- struct{}{}:
	default: // already awake; it will find the job when it looks
	}
}

func (q *decodeQueue) notify(statuses []decodeStatus) {
	if q.onChange != nil {
		q.onChange(statuses)
	}
}

// statusesLocked snapshots the queue for onChange. Running jobs first (the
// innermost one is the one actually decoding), then everything pending in
// the order take will pull it: shortest first. Callers hold q.mu.
func (q *decodeQueue) statusesLocked() []decodeStatus {
	out := make([]decodeStatus, 0, len(q.active)+len(q.pending))
	for i, job := range q.active {
		out = append(out, decodeStatus{
			Label:      job.label,
			Kind:       kindTranscribe,
			Key:        job.key,
			Seconds:    job.seconds,
			Running:    i == len(q.active)-1,
			QueuedAtMS: job.queuedAt.UnixMilli(),
		})
	}
	rest := make([]*decodeJob, len(q.pending))
	copy(rest, q.pending)
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].seconds < rest[j].seconds })
	for _, job := range rest {
		out = append(out, decodeStatus{
			Label:      job.label,
			Kind:       kindTranscribe,
			Key:        job.key,
			Seconds:    job.seconds,
			QueuedAtMS: job.queuedAt.UnixMilli(),
		})
	}
	return out
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
	q.active = append(q.active, job)
	statuses := q.statusesLocked()
	q.mu.Unlock()
	q.notify(statuses)

	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC during transcription: %v\n%s", r, debug.Stack())
			notifyPane("Transcription failed unexpectedly. See ~/Library/Logs/Voxlog.log", ui.PaneHistory)
		}
		q.mu.Lock()
		q.running = outer
		q.inFlight--
		for i := len(q.active) - 1; i >= 0; i-- {
			if q.active[i] == job {
				q.active = append(q.active[:i], q.active[i+1:]...)
				break
			}
		}
		statuses := q.statusesLocked()
		q.mu.Unlock()
		q.notify(statuses)
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

// republish re-sends the current queue without anything having changed in
// it. The LLM jobs live outside this queue but ride the same notification
// (see app.trackLLM), and this is how they get one.
func (q *decodeQueue) republish() {
	q.mu.Lock()
	statuses := q.statusesLocked()
	q.mu.Unlock()
	q.notify(statuses)
}

// outstanding is how many takes are queued or running. The background
// backfill reads it to stay out of the way: it only starts re-reading an old
// meeting when nothing the user is waiting for is in the queue.
func (q *decodeQueue) outstanding() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inFlight
}
