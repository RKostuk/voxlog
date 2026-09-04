package hotkey

/*
#cgo LDFLAGS: -framework ApplicationServices
#include <ApplicationServices/ApplicationServices.h>

// CGEventTapCreate wants a plain C function pointer, which a Go closure can
// never satisfy. This trampoline is the fixed C-side target: it forwards
// every tap event to the Go-exported callback below, which dispatches to
// whichever *Listener is currently active (see activeListener). The
// trampoline's body lives in trampoline.c, not here - see that file for why.
extern CGEventRef hotkeyEventTapTrampoline(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *refcon);
*/
import "C"

import (
	"errors"
	"runtime"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// tapDetector fires its callback when its target key is pressed and released
// without any other key going down in between — a "standalone tap". Mirrors
// hotkeys.py's _TapDetector exactly: pressing the target key (re)arms it and
// clears the "other key seen" flag; any other key-down while armed marks the
// tap as combined; release only fires the callback if no other key intervened.
type tapDetector struct {
	target       KeyID
	callback     func()
	held         bool
	otherPressed bool
}

func (d *tapDetector) onPress(kid KeyID) {
	if kid == d.target {
		d.held = true
		d.otherPressed = false
	} else if d.held {
		d.otherPressed = true
	}
}

func (d *tapDetector) onRelease(kid KeyID) {
	if kid != d.target {
		return
	}
	d.held = false
	if !d.otherPressed && d.callback != nil {
		d.callback()
	}
}

// Callbacks is what a Listener reports. Dictate has two shapes because the
// dictate key has two: a tap that toggles, or a hold that records for exactly
// as long as it is down.
type Callbacks struct {
	// Dictate fires once per tap, in toggle mode.
	Dictate func()
	// DictateDown and DictateUp are the two edges of a held key, in hold mode.
	DictateDown func()
	DictateUp   func()

	History func()
	Meeting func()
	// Escape fires on any Escape key-down, not a standalone tap: cancelling an
	// in-progress recording should work even mid-chord, and unlike the other
	// bindings it is fixed rather than user-configurable.
	Escape func()

	// Hold reports whether the dictate key is currently a push-to-talk key.
	// Asked per event rather than read once at startup, so changing the mode
	// in Settings applies to the very next press.
	Hold func() bool
}

// Listener taps global key events and reports the configured dictate, history
// and meeting bindings. A plain key is watched by tap detection (press and
// release with nothing in between, which is what makes a bare right-Command
// binding usable); a combination is matched against the modifiers held when
// its key goes down.
type Listener struct {
	dictateBinding Binding
	historyBinding Binding
	meetingBinding Binding
	// The tapDetector is nil for combinations and vice versa.
	dictate *tapDetector
	history *tapDetector
	meeting *tapDetector
	cb      Callbacks

	// dictateHeld tracks a push-to-talk key that is currently down, so the
	// release can be recognized however it arrives -- the key itself going up,
	// or a modifier the binding needs being let go first.
	dictateHeld bool

	ready chan struct{} // closed once Start has attempted setup (success or failure)

	mu      sync.Mutex
	port    C.CFMachPortRef
	runSrc  C.CFRunLoopSourceRef
	runLoop C.CFRunLoopRef
}

// escapeKeycode is Escape's macOS virtual keycode.
const escapeKeycode = "53"

func NewListener(dictateKey, historyKey, meetingKey Binding, cb Callbacks) *Listener {
	l := &Listener{
		dictateBinding: dictateKey,
		historyBinding: historyKey,
		meetingBinding: meetingKey,
		cb:             cb,
		ready:          make(chan struct{}),
	}
	if !dictateKey.HasMods() {
		l.dictate = &tapDetector{target: dictateKey.Key, callback: cb.Dictate}
	}
	if !historyKey.HasMods() {
		l.history = &tapDetector{target: historyKey.Key, callback: cb.History}
	}
	if !meetingKey.IsZero() && !meetingKey.HasMods() {
		l.meeting = &tapDetector{target: meetingKey.Key, callback: cb.Meeting}
	}
	return l
}

// holdMode reports whether the dictate key is push-to-talk right now.
func (l *Listener) holdMode() bool { return l.cb.Hold != nil && l.cb.Hold() }

// handleDictateHold applies one key transition to a push-to-talk dictate key.
//
// Unlike the tap detector this deliberately ignores other keys pressed in
// between: the whole point of holding a key while speaking is that the hands
// are free to keep working, and a bare modifier binding is pressed alongside
// other keys constantly.
func (l *Listener) handleDictateHold(kid KeyID, mods []string, isPress bool) {
	b := l.dictateBinding
	if b.IsZero() {
		return
	}

	if isPress {
		match := kid == b.Key
		if b.HasMods() {
			// A combination has to be complete. A plain key is compared
			// directly instead, because a bare modifier binding sets its own
			// flag -- right Command "held with Command" would never match.
			match = b.Matches(kid, mods)
		}
		// Guarded against auto-repeat, which delivers a key-down every few
		// tens of milliseconds for one physical hold.
		if match && !l.dictateHeld {
			l.dictateHeld = true
			if l.cb.DictateDown != nil {
				l.cb.DictateDown()
			}
		}
		return
	}

	if !l.dictateHeld {
		return
	}
	// Either the bound key came up, or a modifier the binding needs did --
	// letting go of Control while still holding D ends the take, which is what
	// "hold this to talk" means for a combination.
	if kid == b.Key || !holdsAll(mods, b.Mods) {
		l.dictateHeld = false
		if l.cb.DictateUp != nil {
			l.cb.DictateUp()
		}
	}
}

// holdsAll reports whether every modifier in need is still held.
func holdsAll(held, need []string) bool {
	for _, m := range need {
		found := false
		for _, h := range held {
			if h == m {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// activeListener is a package-level singleton because CGEventTapCreate's
// callback is a bare C function pointer with no way to carry a Go closure or
// receiver; the trampoline looks the live *Listener up here instead. Only one
// tap runs at a time in this app, so a single slot (not a registry keyed by
// refcon) is enough.
var (
	activeMu       sync.Mutex
	activeListener *Listener
)

// handleEvent applies one already-classified key transition to both
// detectors. isPress's meaning for a FlagsChanged event (a modifier key)
// must be computed by the caller via modifierKeyDown, once per event -- it
// depends on comparing against the previous event's flags, so computing it
// twice (once here, once in the capture check) would see the second call's
// "previous" already overwritten by the first and misreport press/release.
func (l *Listener) handleEvent(event C.CGEventRef, isPress bool) {
	keycode := int(C.CGEventGetIntegerValueField(event, C.kCGKeyboardEventKeycode))
	kid := KeyID{Kind: "vk", Value: strconv.Itoa(keycode)}

	if isPress && kid.Value == escapeKeycode && kid.Kind == "vk" && l.cb.Escape != nil {
		l.cb.Escape()
	}

	mods := heldModifiers(event)

	// A held dictate key needs both edges, so it takes the key over entirely
	// -- neither the chord match nor the tap detector below applies to it.
	if l.holdMode() {
		l.handleDictateHold(kid, mods, isPress)
	} else if isPress && l.dictateBinding.HasMods() && l.dictateBinding.Matches(kid, mods) && l.cb.Dictate != nil {
		// The event carries which modifiers were down when it happened, so a
		// combination needs no held-key bookkeeping of its own -- and it fires
		// on the key going down, like every other shortcut on the system,
		// rather than waiting for a release a chord may never cleanly produce.
		l.cb.Dictate()
	}

	if isPress {
		if l.historyBinding.HasMods() && l.historyBinding.Matches(kid, mods) && l.cb.History != nil {
			l.cb.History()
		}
		if l.meetingBinding.HasMods() && l.meetingBinding.Matches(kid, mods) && l.cb.Meeting != nil {
			l.cb.Meeting()
		}
	}

	for _, d := range []*tapDetector{l.dictate, l.history, l.meeting} {
		if d == nil || (d == l.dictate && l.holdMode()) {
			continue
		}
		if isPress {
			d.onPress(kid)
		} else {
			d.onRelease(kid)
		}
	}
}

// heldModifiers reports the modifiers set on an event.
func heldModifiers(event C.CGEventRef) []string {
	flags := C.CGEventGetFlags(event)
	var mods []string
	if flags&C.kCGEventFlagMaskControl != 0 {
		mods = append(mods, ModControl)
	}
	if flags&C.kCGEventFlagMaskAlternate != 0 {
		mods = append(mods, ModOption)
	}
	if flags&C.kCGEventFlagMaskShift != 0 {
		mods = append(mods, ModShift)
	}
	if flags&C.kCGEventFlagMaskCommand != 0 {
		mods = append(mods, ModCommand)
	}
	return mods
}

// modifierKeycodes are the keys that only ever act as modifiers. Capturing
// one of these IS the binding (a bare right-Command tap, say), so the flags
// it sets must not be folded into it as its own modifier.
var modifierKeycodes = map[string]bool{
	"54": true, "55": true, "56": true, "57": true, "58": true,
	"59": true, "60": true, "61": true, "62": true, "63": true,
}

// lastModifierFlags is the previous event's raw flags bitmask. Modifier keys
// (Shift/Control/Option/Command, including the default right-Control
// dictate / right-Shift history bindings) never generate kCGEventKeyDown/
// kCGEventKeyUp at all -- only kCGEventFlagsChanged, with no explicit
// "pressed" vs "released" field of its own. Comparing this event's flags
// against the previous event's tells us which: the bit for the keycode
// that changed is set in the new flags on press, cleared on release. Only
// touched from the tap callback, which the CFRunLoop invokes serially on
// one thread, so no mutex is needed here (unlike activeListener/captureCh,
// which are written from Start()/Stop()/CaptureNextKey on other threads).
var lastModifierFlags C.CGEventFlags

func modifierKeyDown(event C.CGEventRef) bool {
	newFlags := C.CGEventGetFlags(event)
	changed := newFlags ^ lastModifierFlags
	lastModifierFlags = newFlags
	return newFlags&changed != 0
}

// captureCh, when non-nil, receives the KeyID of the very next key-down
// seen on the already-running tap — how the Settings window's "press a
// key" binding works, reusing the one listener singleton already permits
// (Start() refuses a second Listener) instead of standing up a separate
// tap just for capture.
var (
	captureMu sync.Mutex
	captureCh chan Binding
	// captureModifier remembers a modifier pressed on its own, so a binding
	// like Control+D is possible at all. Capture used to finish on the first
	// key DOWN, and in any combination that is a modifier -- so pressing
	// Control+D bound plain Control and never saw the D. A modifier is now
	// held here until either a real key follows it (making the combination)
	// or the user lets go without pressing one (making the modifier itself
	// the binding, which is how the default right-Command tap is set).
	captureModifier KeyID
)

// CaptureNextKey blocks until the next key-down arrives on the running
// listener's tap, or timeout elapses first. Only one capture may be in
// flight at a time; a second concurrent call errors immediately rather
// than queuing behind the first.
func CaptureNextKey(timeout time.Duration) (Binding, error) {
	captureMu.Lock()
	if captureCh != nil {
		captureMu.Unlock()
		return Binding{}, errors.New("hotkey: a capture is already in progress")
	}
	ch := make(chan Binding, 1)
	captureCh = ch
	captureMu.Unlock()

	defer func() {
		captureMu.Lock()
		if captureCh == ch {
			captureCh = nil
		}
		captureModifier = KeyID{}
		captureMu.Unlock()
	}()

	select {
	case b := <-ch:
		return b, nil
	case <-time.After(timeout):
		return Binding{}, errors.New("hotkey: no key pressed before timeout")
	}
}

// finishCapture delivers a captured binding and closes the capture down.
func finishCapture(b Binding) {
	captureMu.Lock()
	ch := captureCh
	captureCh = nil
	captureModifier = KeyID{}
	captureMu.Unlock()
	if ch != nil {
		ch <- b
	}
}

//export goHotkeyEventTapCallback
func goHotkeyEventTapCallback(proxy C.CGEventTapProxy, eventType C.CGEventType, event C.CGEventRef, refcon unsafe.Pointer) C.CGEventRef {
	// Classify press vs. release once, here, for both the capture check
	// below and handleEvent: for a regular key that's just which of
	// KeyDown/KeyUp this is; for a modifier (Shift/Control/etc, which never
	// generates KeyDown/KeyUp at all) it's a FlagsChanged event, and
	// modifierKeyDown must be called exactly once per event since it
	// advances lastModifierFlags as a side effect -- a second call here
	// would see its own update as "no change" and misreport every
	// modifier release as a press.
	isPress := eventType == C.kCGEventKeyDown
	if eventType == C.kCGEventFlagsChanged {
		isPress = modifierKeyDown(event)
	}

	captureMu.Lock()
	capturing := captureCh != nil
	captureMu.Unlock()
	if capturing {
		keycode := int(C.CGEventGetIntegerValueField(event, C.kCGKeyboardEventKeycode))
		kid := KeyID{Kind: "vk", Value: strconv.Itoa(keycode)}

		switch {
		case isPress && modifierKeycodes[kid.Value]:
			// Hold it: this may be the start of a combination.
			captureMu.Lock()
			captureModifier = kid
			captureMu.Unlock()

		case isPress:
			// A real key. Whatever modifiers are held with it become part of
			// the binding, so the user simply performs the shortcut.
			finishCapture(Binding{Key: kid, Mods: canonicalMods(heldModifiers(event))})

		case !isPress && modifierKeycodes[kid.Value]:
			// Released without a key following: the modifier itself is the
			// binding.
			captureMu.Lock()
			pending := captureModifier
			captureMu.Unlock()
			if pending == kid {
				finishCapture(Binding{Key: kid})
			}
		}
		return event // routed to the capture channel, not the tap detectors
	}

	activeMu.Lock()
	l := activeListener
	activeMu.Unlock()
	if l != nil {
		l.handleEvent(event, isPress)
	}
	return event // listen-only tap: never consume or modify the event
}

// Start installs the CGEventTap and blocks running its run loop until Stop
// is called. Call it from a dedicated goroutine (not the systray goroutine) —
// it locks the calling goroutine to its OS thread for the duration, since
// CFRunLoopGetCurrent/CFRunLoopRun must observe the same thread.
func (l *Listener) Start() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	activeMu.Lock()
	if activeListener != nil {
		activeMu.Unlock()
		close(l.ready)
		return errors.New("hotkey: a listener is already running")
	}
	activeListener = l
	activeMu.Unlock()
	defer func() {
		activeMu.Lock()
		if activeListener == l {
			activeListener = nil
		}
		activeMu.Unlock()
	}()

	mask := C.CGEventMask(1<<C.kCGEventKeyDown | 1<<C.kCGEventKeyUp | 1<<C.kCGEventFlagsChanged)
	port := C.CGEventTapCreate(
		C.kCGSessionEventTap,
		C.kCGHeadInsertEventTap,
		C.kCGEventTapOptionListenOnly,
		mask,
		C.CGEventTapCallBack(C.hotkeyEventTapTrampoline),
		nil,
	)
	if port == 0 {
		close(l.ready)
		return errors.New("hotkey: failed to create event tap (check Accessibility permission for this app in System Settings)")
	}

	runSrc := C.CFMachPortCreateRunLoopSource(0, port, 0)
	runLoop := C.CFRunLoopGetCurrent()
	C.CFRunLoopAddSource(runLoop, runSrc, C.kCFRunLoopCommonModes)
	C.CGEventTapEnable(port, C._Bool(true))

	l.mu.Lock()
	l.port = port
	l.runSrc = runSrc
	l.runLoop = runLoop
	l.mu.Unlock()
	close(l.ready)

	C.CFRunLoopRun() // returns once CFRunLoopStop(runLoop) fires from Stop()
	return nil
}

// Stop invalidates the tap and stops the run loop Start() is blocked in. Safe
// to call from any goroutine; waits for Start()'s setup to finish (or fail)
// first so there's always a consistent port/run loop to tear down.
func (l *Listener) Stop() {
	<-l.ready

	l.mu.Lock()
	port, runSrc, runLoop := l.port, l.runSrc, l.runLoop
	l.mu.Unlock()

	if port == 0 {
		return // Start() failed before creating a tap; nothing to tear down
	}

	C.CGEventTapEnable(port, C._Bool(false))
	C.CFMachPortInvalidate(port)
	if runSrc != 0 {
		C.CFRunLoopRemoveSource(runLoop, runSrc, C.kCFRunLoopCommonModes)
	}
	if runLoop != 0 {
		C.CFRunLoopStop(runLoop)
	}
}
