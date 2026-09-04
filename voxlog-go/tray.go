package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"voxlog-go/internal/sfsymbol"
	"voxlog-go/internal/ui"
)

// The status item's states. Idle goes back to the template image so the glyph
// follows the menu bar's own appearance; the two busy states are regular
// images, since a template image is drawn from its alpha alone and macOS would
// discard the color.
//
// This is the only feedback a user gets that a take is actually running when
// the overlay is parked in a corner they are not looking at -- and the only
// way to tell "still recording" from "still decoding" at a glance.
//
// Dispatched through ui.RunOnMain rather than called directly: these run
// from whichever goroutine noticed the state change (a hotkey callback, a
// decode-queue worker), and systray's SetIcon/SetTemplateIcon marshal to the
// main thread themselves via a synchronous performSelectorOnMainThread:...
// waitUntilDone:YES. That is a second, independent way onto the main thread
// besides this app's own (ui.RunOnMain's dispatch_async_f) -- contention
// between the two deadlocked the whole app the moment a recording started.
// Going through RunOnMain keeps every hop onto the main thread on the one
// mechanism already proven safe for window creation.
func trayIdle() {
	ui.RunOnMain(func() { systray.SetTemplateIcon(ui.MenuBarIcon, ui.MenuBarIcon) })
}
func trayRecording() {
	ui.RunOnMain(func() { systray.SetIcon(ui.MenuBarIconRecording) })
}
func trayTranscribing() {
	ui.RunOnMain(func() { systray.SetIcon(ui.MenuBarIconTranscribing) })
}

// trayMuted swaps the waveform for a struck-through microphone, in the same
// red the recording glyph uses, for as long as the meeting's microphone is
// muted. Falls back to the recording glyph on a macOS without the symbol,
// which is the honest failure: better the ordinary recording icon than none.
func trayMuted() {
	png := sfsymbol.TintedPNG("mic.slash.fill", menuBarSymbolPoints, 0xE0, 0x3E, 0x3E)
	if png == nil {
		trayRecording()
		return
	}
	ui.RunOnMain(func() { systray.SetIcon(png) })
}

// menuBarSymbolPoints is the size AppKit draws a status item image at.
const menuBarSymbolPoints = 16

// Menu bar states, in the order they take precedence.
const (
	stateRecording    = "recording"
	stateTranscribing = "transcribing"
	stateMeeting      = "meeting"
	stateMuted        = "muted"
	stateIdle         = "idle"
)

// trayStateFor picks what the menu bar shows out of everything true at once.
//
// Derived rather than set: with takes decoding while later ones record, and a
// meeting running under both, "whatever happened last" is wrong. A decode
// finishing must not paint the icon idle while the microphone is open.
//
// Recording outranks a meeting because the meeting is the background state --
// it can last an hour, and the elapsed-time title says so anyway, so the icon
// is free to report the thing that just started.
//
// A muted microphone outranks everything: it is the one state where what the
// menu bar says is happening and what is actually being recorded disagree.
func trayStateFor(recording, decoding int, meeting, micMuted bool) string {
	switch {
	case micMuted:
		return stateMuted
	case recording > 0:
		return stateRecording
	case decoding > 0:
		return stateTranscribing
	case meeting:
		return stateMeeting
	default:
		return stateIdle
	}
}

// elapsedLabel renders how long a meeting has been running, for the menu bar
// title beside the glyph. Minutes and seconds until an hour, then hours --
// a call is followed by the clock, not by a stopwatch to the second.
func elapsedLabel(d time.Duration) string { return "● " + clockLabel(d) }

// clockLabel is the bare running time, for the places that draw their own
// marker beside it (the menu's blinking dot, see blinkMeetingStatus).
func clockLabel(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	h, m, s := total/3600, (total/60)%60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// tray tracks what is going on and pushes it to the menu bar. Everything that
// starts or finishes reports here; nothing calls the icon setters directly.
type tray struct {
	mu           sync.Mutex
	recording    int
	decoding     int
	meeting      bool
	meetingSince time.Time
	micMuted     bool
	last         string
	lastTitle    string

	// onRecording fires when a dictation starts or stops -- when the count
	// crosses zero, not on every take -- so the menu can offer to stop the one
	// that is running. Set once at startup, before anything can record.
	onRecording func(running bool)
}

func (t *tray) startedRecording() {
	t.mu.Lock()
	t.recording++
	first := t.recording == 1
	fn := t.onRecording
	t.mu.Unlock()
	if first && fn != nil {
		fn(true)
	}
	t.refresh()
}

func (t *tray) stoppedRecording() {
	t.mu.Lock()
	// The transition, not the state: an unbalanced stop (a take abandoned
	// before it ever started) must not close the menu's blink channel twice.
	last := t.recording == 1
	if t.recording > 0 {
		t.recording--
	}
	fn := t.onRecording
	t.mu.Unlock()
	if last && fn != nil {
		fn(false)
	}
	t.refresh()
}

// setDecoding takes the queue's count of takes waiting or in flight.
func (t *tray) setDecoding(n int) {
	t.mu.Lock()
	t.decoding = n
	t.mu.Unlock()
	t.refresh()
}

func (t *tray) startedMeeting(at time.Time) {
	t.mu.Lock()
	t.meeting, t.meetingSince, t.micMuted = true, at, false
	t.mu.Unlock()
	t.refresh()
}

func (t *tray) stoppedMeeting() {
	t.mu.Lock()
	t.meeting, t.micMuted = false, false
	t.mu.Unlock()
	t.refresh()
}

// setMicMuted puts a muted microphone in the menu bar itself. A recording
// that is not picking you up is worth a glyph of its own: the mistake it
// prevents -- talking through a call the recorder cannot hear -- is only
// discoverable afterwards, when the recording is all there is.
func (t *tray) setMicMuted(muted bool) {
	t.mu.Lock()
	t.micMuted = muted
	t.mu.Unlock()
	t.refresh()
}

// refresh applies the current state. Cheap to call often -- the ticker behind
// a running meeting calls it every few seconds -- because it only touches the
// status item when something actually changed.
func (t *tray) refresh() {
	t.mu.Lock()
	state := trayStateFor(t.recording, t.decoding, t.meeting, t.micMuted)
	title := ""
	if t.meeting {
		title = elapsedLabel(time.Since(t.meetingSince))
		if t.micMuted {
			// Spelled out beside the clock as well as drawn: the glyph is 16
			// points of struck-through microphone, and this is the line that
			// survives a glance at the wrong moment.
			title += " · mic off"
		}
	}
	changedState := state != t.last
	changedTitle := title != t.lastTitle
	t.last, t.lastTitle = state, title
	t.mu.Unlock()

	if changedState {
		switch state {
		case stateMuted:
			trayMuted()
		case stateRecording, stateMeeting:
			trayRecording()
		case stateTranscribing:
			trayTranscribing()
		default:
			trayIdle()
		}
	}
	if changedTitle {
		ui.RunOnMain(func() { systray.SetTitle(title) })
	}
}
