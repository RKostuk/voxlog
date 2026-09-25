// Package ui implements the three webview windows Voxlog shows the user:
// the always-visible recording overlay, the transcript history list, and
// the settings editor.
package ui

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	webview "github.com/webview/webview_go"
)

// Size the window is created at, before the first Configure replaces it.
// Kept small so a style change grows the frame rather than shrinking it,
// which is the direction that never leaves a stale black rectangle behind.
const (
	overlayWidth  = 84.0
	overlayHeight = 26.0
)

// Overlay position settings, as stored in Settings.OverlayPosition.
const (
	// OverlayAtCursor places the overlay beside the cursor once, when the
	// recording starts, and leaves it there.
	OverlayAtCursor = "cursor"
	// OverlayFollowCursor keeps it beside the cursor for as long as the
	// recording lasts.
	OverlayFollowCursor = "follow"
)

// overlayCorners maps the fixed-position settings values to the corner
// numbering positionInCorner uses.
var overlayCorners = map[string]int{
	"top_left":     0,
	"top_right":    1,
	"bottom_left":  2,
	"bottom_right": 3,
	// The two below sit on the very bottom edge of the screen, in the strip
	// the Dock occupies -- which is empty at both ends -- rather than above
	// it like bottom_left/bottom_right.
	"screen_bottom_left":  4,
	"screen_bottom_right": 5,
}

// OverlayPositions lists every accepted value of the setting, so the
// settings window and this package cannot drift apart over what is offered.
var OverlayPositions = []string{
	OverlayAtCursor, OverlayFollowCursor,
	OverlayTopCentre, OverlayBottomCentre,
	"top_left", "top_right", "bottom_left", "bottom_right",
	"screen_bottom_left", "screen_bottom_right",
}

// The two centred placements. A cuff belongs at OverlayTopCentre -- flush
// under the menu bar, where it meets the notch -- and a teleprompter belongs
// at the bottom, but neither is forced: a style only supplies the default,
// and the user moves it from there.
const (
	OverlayTopCentre    = "top_centre"
	OverlayBottomCentre = "bottom_centre"
)

// IndicatorStyle is one layout of the recording indicator. The window is a
// bounding box, not the shape: the page draws a content-sized indicator and
// aligns it inside, so the black of a cuff ends where its content ends
// rather than filling the frame Go asked for.
type IndicatorStyle struct {
	Width, Height float64
	// DefaultPosition is where this style wants to be. Only a default: the
	// user can move any style anywhere, and Settings offers the whole list.
	// It is what a freshly chosen style starts at, so picking "cuff" puts it
	// against the notch without anyone having to know that is where it goes.
	DefaultPosition string
	// StreamingWidth widens the window while a live transcript is showing.
	// Zero means the style already has room for text and never resizes.
	StreamingWidth float64
}

// IndicatorStyles is every layout the overlay can draw, keyed by the value
// stored in Settings.IndicatorStyle. Adding one is a CSS block in
// overlay.html plus a row here -- there is no third place to touch.
var IndicatorStyles = map[string]IndicatorStyle{
	// State, level, mode. A rounded pill anywhere on screen -- except at the
	// top centre, where it squares its shoulders and meets the menu bar, so
	// it reads as the notch growing rather than a window appearing. That was
	// a separate style once; it is the same object in a different place.
	// Height carries room for the caption panel under the pill (see
	// indicator.css's .style-capsule .live) even though the pill itself is 32px
	// -- the window never resizes for streaming text (StreamingWidth: 0), so the
	// room has to already be there, invisible until a live transcript appears.
	// Two lines of caption, because the panel is out of flow now: it cannot
	// push the window taller the way the old wrapped line did, it just clips.
	"capsule": {Width: 460, Height: 96, DefaultPosition: OverlayTopCentre, StreamingWidth: 0},
	// The words themselves, with the level as a band under them.
	"transcript": {Width: 430, Height: 140, DefaultPosition: OverlayBottomCentre},
}

// DefaultIndicatorStyle is what an unrecognized setting falls back to, so a
// settings file from a newer build can never leave the overlay unsizable.
const DefaultIndicatorStyle = "capsule"

func styleOrDefault(name string) (string, IndicatorStyle) {
	if s, ok := IndicatorStyles[name]; ok {
		return name, s
	}
	return DefaultIndicatorStyle, IndicatorStyles[DefaultIndicatorStyle]
}

// IndicatorParts is which pieces of the indicator are mounted. Every style
// is the same markup with parts turned on and off; this is that switchboard,
// handed to the page as a class list.
type IndicatorParts struct {
	Wave, Timer, ModeLabel, StopButton, Solid, Outline bool
	// WaveWidth/WaveHeight size the level meter, in points. Not classes:
	// they are a continuous choice, so they go to the page as CSS variables
	// (see indicator.css's --wave-w/--wave-h). Zero means "use the default",
	// which is what a settings file written before the sliders existed has.
	WaveWidth, WaveHeight int
}

// Bounds for the wave sliders. The upper end is what still fits the capsule's
// window (460 wide) once a mode label and a shortcut are beside it.
const (
	waveMinWidth, waveMaxWidth   = 40, 280
	waveMinHeight, waveMaxHeight = 8, 40
	waveDefWidth, waveDefHeight  = 96, 16
)

func clampWave(v, min, max, def int) int {
	if v == 0 {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// classList renders the parts as the `on-*` classes overlay.html keys off.
// Building the string here rather than in JS keeps the page free of any
// knowledge of the settings struct.
func (p IndicatorParts) classList(style, position string) string {
	// The placement is a class too: the capsule changes shape when it meets
	// the menu bar, and the page cannot know that from the style alone.
	out := "style-" + style + " at-" + position
	for _, c := range []struct {
		on   bool
		name string
	}{
		{p.Wave, "on-wave"},
		{p.Timer, "on-timer"},
		{p.ModeLabel, "on-mode"},
		{p.StopButton, "on-stop"},
		{p.Solid, "on-solid"},
		{p.Outline, "on-outline"},
	} {
		if c.on {
			out += " " + c.name
		}
	}
	return out
}

// followInterval is how often the overlay is repositioned in follow mode.
// 30fps: fast enough that the capsule reads as attached to the pointer,
// while each step is only a couple of objc_msgSends.
const followInterval = 33 * time.Millisecond

// Overlay shows/hides a small recording indicator window. Where it appears
// is the user's choice (see the position constants above); by default it is
// placed beside the cursor once per recording rather than tracking it.
type Overlay struct {
	w      webview.WebView
	ready  chan struct{}
	mu     sync.Mutex
	closed bool

	// posMu guards everything the follow goroutine touches: the chosen
	// style and position, the current window size (which changes when a live
	// transcript widens the capsule), and the goroutine's own stop channel.
	posMu      sync.Mutex
	style      IndicatorStyle
	position   string
	width      float64
	height     float64
	stopFollow chan struct{}
}

// NewOverlay creates the overlay window, hidden, off-screen until the
// first Show() positions it beside the cursor.
//
// Window construction must happen on the real OS main thread via runOnMain
// (see mainthread.go) -- AppKit aborts the process if NSWindow/WKWebView
// objects are created off that thread, which a bare `go o.run()` here used
// to do. NewOverlay still blocks the caller until the window exists
// (o.ready), it just no longer owns the goroutine that creates it.
func NewOverlay() *Overlay {
	_, st := styleOrDefault(DefaultIndicatorStyle)
	o := &Overlay{
		ready:    make(chan struct{}),
		position: st.DefaultPosition,
		style:    st,
		width:    st.Width,
		height:   st.Height,
	}
	runOnMain(func() { o.run() })
	<-o.ready
	return o
}

// Configure sets the indicator's layout, its mounted parts, and where it
// sits. Takes effect on the next
// Show, which is why the app calls this per dictation from the settings it
// already has in hand rather than wiring up a change notification.
//
// The window is resized here rather than in Show: the page needs the frame it
// is going to be drawn in before it lays anything out, and a style change is
// rare enough that doing it eagerly costs nothing.
func (o *Overlay) Configure(styleName string, parts IndicatorParts, position string) {
	name, st := styleOrDefault(styleName)

	o.posMu.Lock()
	o.style = st
	o.width = st.Width
	o.height = st.Height
	o.position = normalizePosition(position, st)
	o.posMu.Unlock()

	if o.w == nil || o.isClosed() {
		return
	}
	o.posMu.Lock()
	placed := o.position
	o.posMu.Unlock()
	classes := parts.classList(name, placed)
	runOnMain(func() {
		resizeWindow(o.w.Window(), st.Width, st.Height)
		// A style carrying a button has to be clickable, and a click-through
		// window can never deliver one. Everything else stays click-through,
		// which is what keeps the indicator from stealing a click from the app
		// the user is actually typing into.
		setIgnoresMouseEvents(o.w.Window(), !parts.StopButton)
		payload, err := json.Marshal(classes)
		if err != nil {
			return
		}
		o.w.Eval(fmt.Sprintf(
			"window.voxlog && window.voxlog.setStyle && window.voxlog.setStyle(%s, %d, %d)",
			payload,
			clampWave(parts.WaveWidth, waveMinWidth, waveMaxWidth, waveDefWidth),
			clampWave(parts.WaveHeight, waveMinHeight, waveMaxHeight, waveDefHeight)))
	})
}

// normalizePosition maps anything unrecognized back to where the style
// wants to be, so a settings file written by a newer build (or edited by
// hand) can never leave the overlay unplaceable. Falling back to the style
// default rather than to the cursor matters now that styles differ: a
// teleprompter appearing beside the pointer would cover the text it is
// describing.
func normalizePosition(position string, style IndicatorStyle) string {
	switch position {
	case OverlayAtCursor, OverlayFollowCursor, OverlayTopCentre, OverlayBottomCentre:
		return position
	}
	if _, ok := overlayCorners[position]; ok {
		return position
	}
	if style.DefaultPosition != "" {
		return style.DefaultPosition
	}
	return OverlayAtCursor
}

// SetPosition chooses where the overlay appears. An unrecognized value falls
// back to the cursor, so a settings file written by a newer build (or edited
// by hand) can never leave the overlay unplaceable. Takes effect on the next
// Show, which is why the app sets it from the current settings on every
// dictation rather than wiring up a change notification.
func (o *Overlay) SetPosition(position string) {
	o.posMu.Lock()
	o.position = normalizePosition(position, o.style)
	o.posMu.Unlock()
}

// place moves the window to wherever the current style -- or, for the one
// style that leaves the choice open, the current setting -- says it goes.
// Runs on the webview's dispatch queue, like every other window mutation.
func (o *Overlay) place() {
	o.posMu.Lock()
	position := o.position
	width, height := o.width, o.height
	o.posMu.Unlock()

	switch position {
	case OverlayTopCentre:
		positionCentred(o.w.Window(), width, height, true)
		return
	case OverlayBottomCentre:
		positionCentred(o.w.Window(), width, height, false)
		return
	}

	if corner, ok := overlayCorners[position]; ok {
		positionInCorner(o.w.Window(), width, height, corner)
		return
	}
	// Both cursor modes start at the cursor; follow mode simply keeps doing
	// this on a ticker for as long as the overlay is up.
	positionNearCursor(o.w.Window(), width, height)
}

// startFollow runs the reposition ticker for follow mode. Idempotent: a
// second Show without an intervening Hide replaces the running ticker rather
// than leaving two of them fighting over the window.
func (o *Overlay) startFollow() {
	o.stopFollowing()

	stop := make(chan struct{})
	o.posMu.Lock()
	o.stopFollow = stop
	o.posMu.Unlock()

	go func() {
		ticker := time.NewTicker(followInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if o.isClosed() {
					return
				}
				runOnMain(func() { o.place() })
			}
		}
	}()
}

func (o *Overlay) stopFollowing() {
	o.posMu.Lock()
	stop := o.stopFollow
	o.stopFollow = nil
	o.posMu.Unlock()
	if stop != nil {
		close(stop)
	}
}

func (o *Overlay) run() {
	w := webview.New(false)
	w.SetTitle("Voxlog")
	// HintNone, not HintFixed: HintFixed pins min==max content size, which
	// then fights the explicit setFrame: this overlay does on every show
	// (see positionNearCursorC). Size is owned natively here instead.
	w.SetSize(int(overlayWidth), int(overlayHeight), webview.HintNone)

	html, err := assets.ReadFile("assets/overlay.html")
	if err != nil {
		panic(err)
	}
	w.SetHtml(injectAsset(string(html), indicatorCSSMarker, "indicator.css"))

	o.w = w
	close(o.ready)

	// Restyle from webview_go's default titled, normal-level window into a
	// borderless floating HUD panel before it's ever shown.
	makeOverlayPanel(w.Window())

	// Start actually hidden (not just its content) until Show() is called
	// -- see hideWindow's doc comment in mainthread.go for why CSS alone
	// isn't enough here.
	hideWindow(w.Window())
}

// Show places the overlay according to the current position setting and
// reveals it without stealing keyboard focus. In follow mode it then keeps
// tracking the cursor until Hide; in every other mode it is placed once per
// dictate-hotkey tap and stays put.
func (o *Overlay) Show() {
	if o.w == nil || o.isClosed() {
		return
	}
	o.posMu.Lock()
	// A previous take may have widened the window for live text.
	o.width = o.style.Width
	o.height = o.style.Height
	width, height := o.width, o.height
	follow := o.position == OverlayFollowCursor
	o.posMu.Unlock()

	runOnMain(func() {
		resizeWindow(o.w.Window(), width, height)
		o.place()
		orderFrontRegardless(o.w.Window())
		x, y, w, h, visible := describeWindow(o.w.Window())
		log.Printf("DEBUG overlay.Show: frame=(%.0f,%.0f %.0fx%.0f) visible=%v", x, y, w, h, visible)
	})

	if follow {
		o.startFollow()
	}
}

// SetLevel drives the equalizer bars from the live mic level, where 0 is
// silence and 1 is a loud peak. Cheap enough to call per audio chunk: it
// only hands a number to the page, which does its own smoothing and
// animates on its own frame clock.
func (o *Overlay) SetLevel(level float64) {
	if o.w == nil || o.isClosed() {
		return
	}
	runOnMain(func() {
		o.w.Eval(fmt.Sprintf("window.voxlog && window.voxlog.setLevel && window.voxlog.setLevel(%.3f)", level))
	})
}

// SetShortcut tells the indicator which key stops the take, for the styles
// that show one. Cheap enough to call per dictation, which is where it is
// called from -- the binding can change between takes and the label has no
// business being cached anywhere else.
func (o *Overlay) SetShortcut(label string) {
	if o.w == nil || o.isClosed() {
		return
	}
	payload, err := json.Marshal(label)
	if err != nil {
		return
	}
	runOnMain(func() {
		o.w.Eval(fmt.Sprintf(
			"window.voxlog && window.voxlog.setShortcut && window.voxlog.setShortcut(%s)", payload))
	})
}

// SetElapsed drives the indicator's clock, for the styles that show one.
func (o *Overlay) SetElapsed(seconds float64) {
	if o.w == nil || o.isClosed() {
		return
	}
	runOnMain(func() {
		o.w.Eval(fmt.Sprintf(
			"window.voxlog && window.voxlog.setElapsed && window.voxlog.setElapsed(%.0f)", seconds))
	})
}

// SetText shows a live partial transcript inside the overlay and widens it
// to fit. Only used in streaming mode; a plain recording never calls this
// and keeps the compact bars-only capsule.
func (o *Overlay) SetText(text string) {
	if o.w == nil || o.isClosed() {
		return
	}
	payload, err := json.Marshal(text)
	if err != nil {
		return
	}
	o.posMu.Lock()
	// Only the styles too small for words grow to fit them; the
	// teleprompters were built around text and never resize.
	if o.style.StreamingWidth > 0 {
		o.width = o.style.StreamingWidth
	}
	width, height := o.width, o.height
	o.posMu.Unlock()

	runOnMain(func() {
		resizeWindow(o.w.Window(), width, height)
		// Re-place after the resize: resizing keeps the bottom-left origin, so
		// a right-hand corner would otherwise grow the capsule straight off
		// the edge of the screen as soon as the first words arrive.
		o.place()
		o.w.Eval(fmt.Sprintf("window.voxlog && window.voxlog.setText && window.voxlog.setText(%s)", payload))
	})
}

// ClearText drops any live transcript and returns the overlay to its
// compact size, ready for the next recording.
func (o *Overlay) ClearText() {
	if o.w == nil || o.isClosed() {
		return
	}
	o.posMu.Lock()
	o.width = o.style.Width
	width, height := o.width, o.height
	o.posMu.Unlock()

	runOnMain(func() {
		o.w.Eval("window.voxlog && window.voxlog.clearText && window.voxlog.clearText()")
		resizeWindow(o.w.Window(), width, height)
		o.place()
	})
}

// SetTranscribing switches the capsule between the recording equalizer and
// the "decoding" dots, in place -- the overlay stays exactly where it was
// so the wait after a take is visibly a continuation of it.
func (o *Overlay) SetTranscribing(on bool) {
	if o.w == nil || o.isClosed() {
		return
	}
	runOnMain(func() {
		o.w.Eval(fmt.Sprintf("window.voxlog && window.voxlog.setTranscribing && window.voxlog.setTranscribing(%t)", on))
	})
}

// Hide conceals the overlay without destroying the window.
func (o *Overlay) Hide() {
	if o.w == nil || o.isClosed() {
		return
	}
	o.stopFollowing()
	runOnMain(func() {
		hideWindow(o.w.Window())
	})
}

// Close terminates the overlay's window. Safe to call more than once, and
// safe to race against Show/Hide — webview_go's Terminate has no internal
// guard against a second call on an already-destroyed handle, so this
// package supplies one.
func (o *Overlay) Close() {
	if o.w == nil {
		return
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	o.mu.Unlock()
	o.stopFollowing()
	o.w.Terminate()
}

func (o *Overlay) isClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}
