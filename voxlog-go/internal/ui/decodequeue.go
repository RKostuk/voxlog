package ui

import (
	"encoding/json"
	"fmt"
	"sync"
)

// DecodeStatus is one entry in the transcription queue, as the window draws
// it. It mirrors the queue's own type in package main -- which this package
// cannot import (main imports ui) -- so the app converts on the way in.
type DecodeStatus struct {
	// Label names the recording ("Dictation, 14:32"), so a row in History or
	// Meetings can be matched to its queue entry by eye.
	Label string `json:"label"`
	// Kind is "transcribe" or "llm"; Stage is what an LLM job is doing.
	Kind  string `json:"kind"`
	Stage string `json:"stage,omitempty"`
	// Key is the id of the row this take belongs to (a meeting's start, in
	// RFC3339Nano) so the list can mark it as transcribing. "" when there is
	// no row yet -- a dictation's history entry is written after the decode.
	Key string `json:"key"`
	// Seconds is the length of the recording, not an estimate of how long
	// decoding will take: the queue orders by it, and it is the only honest
	// number available before the work is done.
	Seconds float64 `json:"seconds"`
	// Running marks the take actually being decoded. Everything else in the
	// slice is waiting.
	Running bool `json:"running"`
	// QueuedAtMS is when the job was submitted, in Unix milliseconds.
	QueuedAtMS int64 `json:"queued_at_ms"`
}

// decodeQueueMu guards the snapshot below. Separate from winMu: the queue
// publishes from its own goroutine on every job start and finish, and it has
// no business waiting behind whatever is holding the window lock.
var (
	decodeQueueMu   sync.Mutex
	decodeQueueLast []DecodeStatus
)

// PublishDecodeQueue records what is queued and pushes it into the window if
// one is open. The snapshot is kept either way, so a window opened later
// still shows the queue that was already running when it opened (see
// decodeQueueJSON).
func PublishDecodeQueue(list []DecodeStatus) {
	if list == nil {
		list = []DecodeStatus{}
	}
	decodeQueueMu.Lock()
	decodeQueueLast = list
	decodeQueueMu.Unlock()

	payload, err := json.Marshal(list)
	if err != nil {
		return
	}
	js := fmt.Sprintf(
		"window.voxlog = window.voxlog || {}; window.voxlog.decodeQueue = %s; window.voxlog.onDecodeQueue && window.voxlog.onDecodeQueue(window.voxlog.decodeQueue);",
		payload)

	winMu.Lock()
	w := mainWin
	winMu.Unlock()
	if w != nil && windowUsable(w) {
		w.Dispatch(func() { w.Eval(js) })
	}

	// The drawer carries the same banner: it is the window that is up while
	// the user is working, which is exactly when "is that meeting decoding
	// yet" gets asked.
	drawerMu.Lock()
	d := drawerWin
	drawerMu.Unlock()
	if d != nil && windowUsable(d) {
		d.Dispatch(func() { d.Eval(js) })
	}
}

// decodeQueueJSON is the snapshot for a page being built or refreshed.
func decodeQueueJSON() []byte {
	decodeQueueMu.Lock()
	list := decodeQueueLast
	decodeQueueMu.Unlock()
	if list == nil {
		list = []DecodeStatus{}
	}
	data, err := json.Marshal(list)
	if err != nil {
		return []byte("[]")
	}
	return data
}
