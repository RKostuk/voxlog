package ui

/*
#cgo LDFLAGS: -framework Cocoa -framework ApplicationServices
#include <dispatch/dispatch.h>
#include <objc/objc.h>
#include <objc/message.h>
#include <objc/runtime.h>
#include <stdint.h>
#include <string.h>
#include <ApplicationServices/ApplicationServices.h>

extern void goMainThreadTrampoline(uintptr_t handle);

// dispatch_async_f wants a plain C function pointer, which a Go closure can
// never satisfy. This trampoline is the fixed C-side target: it forwards to
// the Go-exported callback below, passing the handle through as a plain
// uintptr_t (not void*) so the Go side never needs an unsafe.Pointer(uintptr(...))
// conversion -- runtime/cgo.Handle already is that idiom, cleanly. Same
// C-callback-to-Go-routing shape internal/hotkey/listener_darwin.go uses for
// CGEventTapCreate, adapted for GCD instead of a single global slot since
// multiple dispatches can be in flight at once here.
static void mainThreadTrampolineC(void *ctx) {
	goMainThreadTrampoline((uintptr_t)ctx);
}

static void runOnMainC(uintptr_t handle) {
	dispatch_async_f(dispatch_get_main_queue(), (void *)handle, mainThreadTrampolineC);
}

// activateAppC brings the process to the foreground. webview_go only does
// this itself the *first* time it ever sets NSApp's delegate -- systray
// already claims that delegate slot before onReady ever runs (see
// systray_darwin.m's applicationDidFinishLaunching), so every webview.New()
// call in this app takes webview.h's "delegate already set" branch and
// skips setActivationPolicy:/activateIgnoringOtherApps: entirely. Without
// this, History/Settings windows are created and ordered front within their
// own (never-foregrounded) app layer, so they render behind whatever app
// currently has focus -- indistinguishable from "the menu does nothing".
// Plain objc_msgSend runtime calls, not [obj msg] syntax: cgo compiles this
// preamble as C, not Objective-C, so bracket message sends aren't available.
static void activateAppC(void) {
	id app = ((id (*)(id, SEL))objc_msgSend)(
		(id)objc_getClass("NSApplication"), sel_registerName("sharedApplication"));
	// NSApplicationActivationPolicyAccessory == 1. Deliberately NOT Regular
	// (0), which this used to set: Regular is what puts an icon in the Dock,
	// and a menu bar app that grows a Dock icon the first time you open its
	// history is not what LSUIElement in Info.plist promised. Accessory apps
	// can still be activated and still own a key window, which is all the
	// History and Settings windows need -- the activation below is the part
	// that was actually missing, not the policy change.
	((void (*)(id, SEL, long))objc_msgSend)(
		app, sel_registerName("setActivationPolicy:"), 1L);
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		app, sel_registerName("activateIgnoringOtherApps:"), (BOOL)1);
}

// hideWindowC/showWindowC toggle the actual NSWindow's visibility.
// webview_go's Eval("display:none") only hides the WKWebView's page
// content -- the NSWindow frame it lives in (title bar included) stays
// fully visible on screen, ordered front, the whole time. For the overlay
// that renders as a permanent tiny stray window sitting on the desktop
// even while "hidden". orderOut:/makeKeyAndOrderFront: are the real
// AppKit show/hide.
static void hideWindowC(void *window) {
	((void (*)(id, SEL, id))objc_msgSend)((id)window, sel_registerName("orderOut:"), nil);
}
static void showWindowC(void *window) {
	((void (*)(id, SEL, id))objc_msgSend)((id)window, sel_registerName("makeKeyAndOrderFront:"), nil);
}

// positionNearCursorC places window once, to the right of and vertically
// centered on wherever the mouse currently is, with a fixed margin -- it
// does NOT track the cursor afterward (unlike the Python original's
// per-tick lerp-follow); the overlay is meant to be glanced at, not chased.
// CGEventGetLocation returns Quartz's top-left-origin display coordinates;
// NSWindow's setFrameOrigin: wants Cocoa's bottom-left-origin, hence the
// flip against the main display's height. Primary-display-only and
// unclamped to screen edges -- acceptable for a small HUD near wherever
// the user is actually typing, not worth the extra multi-monitor code the
// Python version carried for a "minimalistic" port.
static void positionNearCursorC(void *window, double winW, double winH) {
	CGEventRef event = CGEventCreate(NULL);
	CGPoint mouse = CGEventGetLocation(event);
	CFRelease(event);

	double screenH = CGDisplayBounds(CGMainDisplayID()).size.height;
	double margin = 24.0;

	// Set origin AND size together: switching the style mask to borderless
	// (see makeOverlayPanelC) recomputes the frame against the old titled
	// layout and collapses the height to 0 -- a fully "visible" window with
	// nothing to draw, which is what "the overlay never appears" actually
	// was. Restating the size on every show makes that unrepresentable.
	// NSRect is layout-identical to CGRect, passed as one struct rather
	// than loose doubles so this doesn't lean on arm64 register accidents.
	CGRect frame;
	frame.origin.x = mouse.x + margin;
	frame.origin.y = screenH - mouse.y - winH / 2.0;
	frame.size.width = winW;
	frame.size.height = winH;

	((void (*)(id, SEL, CGRect, BOOL))objc_msgSend)(
		(id)window, sel_registerName("setFrame:display:"), frame, (BOOL)1);
}

// positionInCornerC pins window to one corner of the screen: 0 top-left,
// 1 top-right, 2 bottom-left, 3 bottom-right.
//
// Measured against visibleFrame rather than frame, so the overlay clears the
// menu bar at the top and the Dock at the bottom instead of hiding underneath
// them. visibleFrame is already in Cocoa's bottom-left-origin coordinates, so
// unlike positionNearCursorC there is no flip to do here.
// Corners 4 and 5 measure against the FULL screen frame instead, putting the
// capsule on the very bottom edge -- the strip the Dock sits in, which is
// empty at both ends unless the Dock is enormous. That space is otherwise
// wasted, and a HUD there overlaps nothing the user is working in.
static void positionInCornerC(void *window, double winW, double winH, int corner) {
	id screen = ((id (*)(id, SEL))objc_msgSend)(
		(id)objc_getClass("NSScreen"), sel_registerName("mainScreen"));
	if (screen == nil) {
		return;
	}
	SEL area = (corner >= 4) ? sel_registerName("frame") : sel_registerName("visibleFrame");
	CGRect vf = ((CGRect (*)(id, SEL))objc_msgSend)(screen, area);

	double margin = 24.0;
	double left = vf.origin.x + margin;
	double right = vf.origin.x + vf.size.width - winW - margin;
	double bottom = vf.origin.y + margin;
	double top = vf.origin.y + vf.size.height - winH - margin;

	CGRect frame;
	frame.origin.x = (corner == 1 || corner == 3 || corner == 5) ? right : left;
	frame.origin.y = (corner == 0 || corner == 1) ? top : bottom;
	frame.size.width = winW;
	frame.size.height = winH;

	((void (*)(id, SEL, CGRect, BOOL))objc_msgSend)(
		(id)window, sel_registerName("setFrame:display:"), frame, (BOOL)1);
}

// positionCentredC pins window to the horizontal centre of the screen,
// either flush under the menu bar or above the Dock.
//
// The top case is what makes a notch-anchored indicator look built in: it is
// measured against the FULL screen frame minus the menu bar's own height, so
// the window's top edge meets the bottom of the menu bar exactly, with no gap
// for the desktop to show through. Deriving the menu bar height from the
// difference between frame and visibleFrame beats NSStatusBar's thickness,
// which does not account for a notch.
//
// The bottom case uses visibleFrame, so it clears the Dock rather than
// hiding under it, plus a margin big enough that the two never touch.
static void positionCentredC(void *window, double winW, double winH, int atTop) {
	id screen = ((id (*)(id, SEL))objc_msgSend)(
		(id)objc_getClass("NSScreen"), sel_registerName("mainScreen"));
	if (screen == nil) {
		return;
	}
	CGRect f = ((CGRect (*)(id, SEL))objc_msgSend)(screen, sel_registerName("frame"));
	CGRect vf = ((CGRect (*)(id, SEL))objc_msgSend)(screen, sel_registerName("visibleFrame"));

	CGRect frame;
	frame.size.width = winW;
	frame.size.height = winH;
	frame.origin.x = f.origin.x + (f.size.width - winW) / 2.0;

	if (atTop) {
		double menuBar = (f.origin.y + f.size.height) - (vf.origin.y + vf.size.height);
		if (menuBar < 0) {
			menuBar = 0;
		}
		frame.origin.y = f.origin.y + f.size.height - menuBar - winH;
	} else {
		frame.origin.y = vf.origin.y + 28.0;
	}

	((void (*)(id, SEL, CGRect, BOOL))objc_msgSend)(
		(id)window, sel_registerName("setFrame:display:"), frame, (BOOL)1);
}

// setIgnoresMouseEventsC decides whether a window is click-through.
//
// The overlay is created click-through (makeOverlayPanelC) so it can never
// steal a click from the app being typed into. An indicator carrying a stop
// button has to give that up -- a window that ignores the mouse cannot
// deliver a press to the button drawn in it.
static void setIgnoresMouseEventsC(void *window, int ignores) {
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		(id)window, sel_registerName("setIgnoresMouseEvents:"), (BOOL)(ignores ? 1 : 0));
}

// appIsActiveC reports whether Voxlog is the frontmost app.
static int appIsActiveC(void) {
	id app = ((id (*)(id, SEL))objc_msgSend)(
		(id)objc_getClass("NSApplication"), sel_registerName("sharedApplication"));
	return ((BOOL (*)(id, SEL))objc_msgSend)(app, sel_registerName("isActive")) ? 1 : 0;
}

// hideOnDeactivateC makes window put itself away the moment the user clicks
// into anything else, the way a popover does, instead of staying up until it
// is explicitly dismissed.
//
// AppKit already implements exactly this (NSWindow.hidesOnDeactivate), which
// beats watching for NSWindowDidResignKeyNotification and ordering the window
// out by hand: the built-in also restores the window if the app is activated
// again with it still "open", and it does not fire for Voxlog's own other
// windows -- clicking Settings does not count as leaving.
//
// The window is only hidden, never closed, so the singleton handle stays
// valid and describeWindow keeps reporting the truth for
// HistoryWindowVisible.
static void hideOnDeactivateC(void *window) {
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		(id)window, sel_registerName("setHidesOnDeactivate:"), (BOOL)1);
}

// makeOverlayPanelC turns a plain webview window into a HUD-style panel:
// no title bar, floating above normal windows, click-through, and visible
// on every Space. webview_go always creates a titled, normal-level window
// (see set_up_window in the vendored webview.h) -- for a recording
// indicator that means a title bar taller than the content and a window
// that disappears behind whatever app the user is typing into, which is
// indistinguishable from "the overlay never showed up".
static void makeOverlayPanelC(void *window) {
	id win = (id)window;
	// NSWindowStyleMaskBorderless == 0
	((void (*)(id, SEL, unsigned long))objc_msgSend)(
		win, sel_registerName("setStyleMask:"), 0UL);
	// NSFloatingWindowLevel == 3
	((void (*)(id, SEL, long))objc_msgSend)(
		win, sel_registerName("setLevel:"), 3L);
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		win, sel_registerName("setOpaque:"), (BOOL)0);
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		win, sel_registerName("setHasShadow:"), (BOOL)0);
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		win, sel_registerName("setIgnoresMouseEvents:"), (BOOL)1);
	// NSWindowCollectionBehaviorCanJoinAllSpaces (1<<0) |
	// NSWindowCollectionBehaviorTransient (1<<3) |
	// NSWindowCollectionBehaviorFullScreenAuxiliary (1<<8)
	((void (*)(id, SEL, unsigned long))objc_msgSend)(
		win, sel_registerName("setCollectionBehavior:"), (1UL << 0) | (1UL << 3) | (1UL << 8));

	id clear = ((id (*)(id, SEL))objc_msgSend)(
		(id)objc_getClass("NSColor"), sel_registerName("clearColor"));
	((void (*)(id, SEL, id))objc_msgSend)(
		win, sel_registerName("setBackgroundColor:"), clear);

	// A transparent NSWindow isn't enough: the WKWebView filling it paints
	// an opaque base layer under the page, so the rounded capsule shows
	// white square corners around it no matter what the page CSS says.
	// Turning that base off is what makes the corners actually transparent.
	// Both selectors below are guarded with respondsToSelector: rather than
	// assumed -- _setDrawsBackground: is private, and skipping it on a
	// future macOS that drops it beats raising an exception.
	id contentView = ((id (*)(id, SEL))objc_msgSend)(win, sel_registerName("contentView"));
	if (contentView != nil) {
		SEL respondsTo = sel_registerName("respondsToSelector:");

		SEL setDrawsBG = sel_registerName("_setDrawsBackground:");
		if (((BOOL (*)(id, SEL, SEL))objc_msgSend)(contentView, respondsTo, setDrawsBG)) {
			((void (*)(id, SEL, BOOL))objc_msgSend)(contentView, setDrawsBG, (BOOL)0);
		}

		// Public API (macOS 12+) covering the overscroll/backstop area.
		SEL setUnderPageBG = sel_registerName("setUnderPageBackgroundColor:");
		if (((BOOL (*)(id, SEL, SEL))objc_msgSend)(contentView, respondsTo, setUnderPageBG)) {
			((void (*)(id, SEL, id))objc_msgSend)(contentView, setUnderPageBG, clear);
		}
	}
}

// makeDrawerPanelC restyles a window into the quick-tasks drawer: no visible
// title bar, floating above normal windows, on every Space -- but, unlike
// makeOverlayPanelC, still able to become key.
//
// That last part is why this is not just makeOverlayPanelC with
// setIgnoresMouseEvents: NO. A borderless NSWindow returns NO from
// canBecomeKeyWindow, so it can never hold the caret, and the drawer's whole
// point is a one-line box you type a task into. Keeping the Titled bit and
// hiding the title bar instead (FullSizeContentView + a transparent,
// invisible title) gets a chromeless window that still takes keyboard input.
static void makeDrawerPanelC(void *window) {
	id win = (id)window;
	// NSWindowStyleMaskTitled (1) | NSWindowStyleMaskFullSizeContentView (1<<15)
	((void (*)(id, SEL, unsigned long))objc_msgSend)(
		win, sel_registerName("setStyleMask:"), 1UL | (1UL << 15));
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		win, sel_registerName("setTitlebarAppearsTransparent:"), (BOOL)1);
	// NSWindowTitleHidden == 1
	((void (*)(id, SEL, long))objc_msgSend)(
		win, sel_registerName("setTitleVisibility:"), 1L);
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		win, sel_registerName("setMovableByWindowBackground:"), (BOOL)1);
	// NSFloatingWindowLevel == 3: the drawer is a companion to whatever the
	// user is working in, not a window they switch to.
	((void (*)(id, SEL, long))objc_msgSend)(
		win, sel_registerName("setLevel:"), 3L);
	// CanJoinAllSpaces (1<<0) | FullScreenAuxiliary (1<<8). No Transient
	// here, unlike the overlay: a transient window is not allowed to become
	// key either.
	((void (*)(id, SEL, unsigned long))objc_msgSend)(
		win, sel_registerName("setCollectionBehavior:"), (1UL << 0) | (1UL << 8));
}

// makeKeyAndOrderFrontC shows the window AND gives it keyboard focus -- the
// opposite of orderFrontRegardlessC below, and what the drawer needs so the
// "add a task" box can be typed into the moment it appears.
static void makeKeyAndOrderFrontC(void *window) {
	((void (*)(id, SEL, id))objc_msgSend)(
		(id)window, sel_registerName("makeKeyAndOrderFront:"), nil);
}

// orderFrontRegardlessC shows the window without stealing focus from
// whatever the user is typing into -- makeKeyAndOrderFront: would yank the
// caret away mid-dictation, which is exactly the wrong behavior for a HUD.
static void orderFrontRegardlessC(void *window) {
	((void (*)(id, SEL))objc_msgSend)(
		(id)window, sel_registerName("orderFrontRegardless"));
}

// resizeWindowC changes a window's size, keeping its top-left corner put.
// Cocoa frames are bottom-left anchored, so growing without compensating
// would make the window appear to slide downward.
static void resizeWindowC(void *window, double w, double h) {
	id win = (id)window;
	CGRect frame = ((CGRect (*)(id, SEL))objc_msgSend)(win, sel_registerName("frame"));
	frame.origin.y += frame.size.height - h;
	frame.size.width = w;
	frame.size.height = h;
	((void (*)(id, SEL, CGRect, BOOL))objc_msgSend)(
		win, sel_registerName("setFrame:display:"), frame, (BOOL)1);
}

// keepAliveOnCloseC makes the window survive its own close button.
//
// NSWindow defaults releasedWhenClosed to YES for windows built with
// initWithContentRect: -- so clicking the red X deallocates it, leaving
// webview_go (and this package's singleton) holding a dangling pointer.
// Re-showing that pointer silently does nothing, which is exactly the
// "History/Settings won't open a second time" symptom. With this off, the
// close button just orders the window out and the handle stays valid, so
// the next Show can bring the very same window straight back.
static void keepAliveOnCloseC(void *window) {
	((void (*)(id, SEL, BOOL))objc_msgSend)(
		(id)window, sel_registerName("setReleasedWhenClosed:"), (BOOL)0);
}

// frontmostAppPidC returns the pid of whatever app is frontmost right now,
// or 0 if that can't be determined.
//
// Used to remember where the user actually was before the History window
// took focus, so a clicked entry can be pasted back into that app. The
// alternative -- a non-activating panel that never takes focus at all --
// was tried and doesn't work: AppKit swallows the first click into a
// non-key window (acceptsFirstMouse is NO by default) so the WKWebView
// never sees it, leaving both the entry clicks and the close button dead.
static int frontmostAppPidC(void) {
	id ws = ((id (*)(id, SEL))objc_msgSend)(
		(id)objc_getClass("NSWorkspace"), sel_registerName("sharedWorkspace"));
	if (ws == nil) return 0;
	id app = ((id (*)(id, SEL))objc_msgSend)(ws, sel_registerName("frontmostApplication"));
	if (app == nil) return 0;
	return (int)((int (*)(id, SEL))objc_msgSend)(app, sel_registerName("processIdentifier"));
}

// frontmostAppNameC copies the frontmost app's localized name into buf.
// Empty when there is no frontmost app or it has no name -- the caller then
// simply has no exclusion to match against, which is the safe direction for
// a feature that decides whether to listen.
static void frontmostAppNameC(char *buf, int cap) {
	if (cap > 0) buf[0] = '\0';
	id ws = ((id (*)(id, SEL))objc_msgSend)(
		(id)objc_getClass("NSWorkspace"), sel_registerName("sharedWorkspace"));
	if (ws == nil) return;
	id app = ((id (*)(id, SEL))objc_msgSend)(ws, sel_registerName("frontmostApplication"));
	if (app == nil) return;
	id name = ((id (*)(id, SEL))objc_msgSend)(app, sel_registerName("localizedName"));
	if (name == nil) return;
	const char *utf8 = ((const char *(*)(id, SEL))objc_msgSend)(name, sel_registerName("UTF8String"));
	if (utf8 == NULL) return;
	strncpy(buf, utf8, (size_t)cap - 1);
	buf[cap - 1] = '\0';
}

// activateAppByPidC brings the app with the given pid back to the front.
static void activateAppByPidC(int pid) {
	if (pid <= 0) return;
	id app = ((id (*)(id, SEL, int))objc_msgSend)(
		(id)objc_getClass("NSRunningApplication"),
		sel_registerName("runningApplicationWithProcessIdentifier:"), pid);
	if (app == nil) return;
	// NSApplicationActivateIgnoringOtherApps == 1 << 1
	((void (*)(id, SEL, unsigned long))objc_msgSend)(
		app, sel_registerName("activateWithOptions:"), 1UL << 1);
}

// describeWindowC fills out[0..4] with x, y, width, height, isVisible --
// diagnostics for "the overlay didn't appear", where the interesting
// question is whether it's hidden, empty, or just parked off-screen.
static void describeWindowC(void *window, double *out) {
	// arm64 returns a CGRect (4-double HFA) in v0-v3 through plain
	// objc_msgSend; objc_msgSend_stret is x86_64-only and absent here.
	CGRect frame = ((CGRect (*)(id, SEL))objc_msgSend)(
		(id)window, sel_registerName("frame"));
	out[0] = frame.origin.x;
	out[1] = frame.origin.y;
	out[2] = frame.size.width;
	out[3] = frame.size.height;
	out[4] = ((BOOL (*)(id, SEL))objc_msgSend)(
		(id)window, sel_registerName("isVisible")) ? 1.0 : 0.0;
}
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// runOnMain schedules f to run on the real OS main thread -- the one
// systray.Run() occupies via Cocoa's run loop (systray's Register()/Run()
// call runtime.LockOSThread() in init() and run onReady on a *separate*
// goroutine specifically so the locked OS thread stays free to pump that
// loop; see github.com/getlantern/systray@v1.2.2/systray.go). f runs
// asynchronously -- runOnMain does not wait for it to start or finish.
//
// AppKit/WebKit object creation (NSWindow, WKWebView -- everything
// webview.New's cocoa_wkwebview_engine constructor does) must happen on
// that thread or the process aborts with "NSWindow should only be
// instantiated on the main thread!". webview_go's own SetTitle/SetSize/
// Init/SetHtml/Bind/Terminate calls are plain unguarded Objective-C message
// sends too, so the whole window construction/setup is run inside a single
// runOnMain closure per window.
//
// IMPORTANT: none of this app's windows ever call webview.Run() (its own
// blocking [NSApp run] event loop) -- systray.Run() is the one and only
// [NSApp run] for the whole process. dispatch_async_f's target queue is
// serial: an earlier version of this code had the overlay's runOnMain
// closure call webview.Run() and never return, which permanently starved
// every later runOnMain dispatch (History/Settings windows silently never
// appeared, no error anywhere -- the queue was just stuck on the first,
// forever-blocked item). Window visibility doesn't need Run() anyway: per
// the vendored webview.h, set_up_window() calls makeKeyAndOrderFront:
// during construction, independent of run_impl().
//
// This is also the app's replacement for webview_go's own w.Dispatch, which
// every main-thread hop in this package used to go through. Dispatch keeps
// its pending closures in one process-wide map keyed by a counter and calls
// whatever it finds there without checking (webview.go:221) -- so a callback
// that arrives for an index the map no longer holds calls a nil func, and the
// process dies with a nil-pointer panic inside
// _webviewDispatchGoCallback. That crash is in the log for 2026-09-24
// 00:38. runtime/cgo.Handle has no shared map and no reuse, so the same
// mistake is not representable here.
func runOnMain(f func()) {
	h := cgo.NewHandle(f)
	C.runOnMainC(C.uintptr_t(h))
}

// RunOnMain is runOnMain, exported for callers outside this package (the
// menu bar icon in package main) that need the same main-thread hop.
// getlantern/systray's own setters (SetIcon/SetTemplateIcon/SetTitle) reach
// the main thread through their own performSelectorOnMainThread:...
// waitUntilDone:YES, a second, independent path onto the same thread this
// package's runOnMainC already serves through dispatch_async_f. Two
// mechanisms racing for the same thread is exactly the shape of bug the
// comment above runOnMain describes having already happened once; routing
// tray updates through this one instead retires the second path rather than
// debugging its interaction with the first.
func RunOnMain(f func()) { runOnMain(f) }

// runOnMainSync runs f on the main thread and waits for the answer, for the
// AppKit questions whose answer decides what the caller does next ("is Voxlog
// frontmost?", "is the window on screen?"). Asking those from a goroutine
// directly is the freeze this exists to prevent: AppKit takes its own locks,
// and the main thread stalls behind them at the next redraw.
//
// NEVER call this from the main thread. The dispatch queue is serial, so
// waiting on it from inside it waits forever -- see runOnMain's comment about
// the first time this queue was blocked on itself.
func runOnMainSync(f func()) {
	done := make(chan struct{})
	runOnMain(func() {
		defer close(done)
		f()
	})
	<-done
}

// activateApp brings Voxlog to the foreground. Must be called on the main
// thread (call it from inside a runOnMain closure, same as window creation).
func activateApp() {
	C.activateAppC()
}

// hideWindow/showWindow toggle a window's actual on-screen visibility
// (not just its page content). Safe to call from any goroutine --
// webview_go's own Objective-C message sends for SetTitle/SetSize/etc.
// aren't main-thread-guarded either, per the vendored source, so this
// matches that existing (if fragile) pattern rather than inventing a
// stricter one just for these two calls.
func hideWindow(window unsafe.Pointer) {
	C.hideWindowC(window)
}
func showWindow(window unsafe.Pointer) {
	C.showWindowC(window)
}

// positionNearCursor moves window once to sit beside the current mouse
// location. See positionNearCursorC's doc comment for the coordinate-flip
// and single-shot (not cursor-tracking) rationale.
func positionNearCursor(window unsafe.Pointer, w, h float64) {
	C.positionNearCursorC(window, C.double(w), C.double(h))
}

// positionCentred pins window to the horizontal centre of the screen, flush
// under the menu bar when atTop, above the Dock otherwise. See
// positionCentredC for how the menu bar height is measured.
func positionCentred(window unsafe.Pointer, w, h float64, atTop bool) {
	top := C.int(0)
	if atTop {
		top = C.int(1)
	}
	C.positionCentredC(window, C.double(w), C.double(h), top)
}

// setIgnoresMouseEvents turns a window's click-through behavior on or off.
// See setIgnoresMouseEventsC for why the overlay ever gives it up.
func setIgnoresMouseEvents(window unsafe.Pointer, ignores bool) {
	v := C.int(0)
	if ignores {
		v = C.int(1)
	}
	C.setIgnoresMouseEventsC(window, v)
}

// positionInCorner pins window to a screen corner. See positionInCornerC for
// the corner numbering and why it measures against the visible frame.
func positionInCorner(window unsafe.Pointer, w, h float64, corner int) {
	C.positionInCornerC(window, C.double(w), C.double(h), C.int(corner))
}

// AppIsActive reports whether Voxlog is the frontmost app. Windows hidden
// because the app was deactivated are indistinguishable from ones the user
// dismissed if you only ask the window, so the toggles ask this too.
//
// Callers are hotkey handlers, i.e. goroutines, so the AppKit call hops to the
// main thread and waits (see runOnMainSync) rather than being made from
// whatever thread asked.
func AppIsActive() bool {
	var active bool
	runOnMainSync(func() { active = C.appIsActiveC() != 0 })
	return active
}

// hideOnDeactivate makes window disappear as soon as the user clicks into
// another app, popover-style. See hideOnDeactivateC.
func hideOnDeactivate(window unsafe.Pointer) {
	C.hideOnDeactivateC(window)
}

// makeOverlayPanel restyles window as a borderless, floating, click-through
// HUD panel. See makeOverlayPanelC's doc comment.
func makeOverlayPanel(window unsafe.Pointer) {
	C.makeOverlayPanelC(window)
}

// orderFrontRegardless shows window without taking keyboard focus.
func orderFrontRegardless(window unsafe.Pointer) {
	C.orderFrontRegardlessC(window)
}

// makeDrawerPanel restyles window as a chromeless floating window that can
// still hold the caret. See makeDrawerPanelC.
func makeDrawerPanel(window unsafe.Pointer) {
	C.makeDrawerPanelC(window)
}

// makeKeyAndOrderFront shows window and gives it keyboard focus.
func makeKeyAndOrderFront(window unsafe.Pointer) {
	C.makeKeyAndOrderFrontC(window)
}

// resizeWindow resizes window in place. See resizeWindowC.
func resizeWindow(window unsafe.Pointer, w, h float64) {
	C.resizeWindowC(window, C.double(w), C.double(h))
}

// keepAliveOnClose stops window from being deallocated when the user closes
// it, so the same window can be re-shown later. See keepAliveOnCloseC.
func keepAliveOnClose(window unsafe.Pointer) {
	C.keepAliveOnCloseC(window)
}

// FrontmostAppName reports the frontmost app's name ("" if unknown). Used by
// always-on listening to honour the exclusion list. Called from a goroutine,
// so the AppKit call hops to the main thread and waits, like every other
// question this file answers.
func FrontmostAppName() string {
	var name string
	runOnMainSync(func() {
		var buf [256]C.char
		C.frontmostAppNameC(&buf[0], C.int(len(buf)))
		name = C.GoString(&buf[0])
	})
	return name
}

// FrontmostAppPid reports which app is frontmost right now (0 if unknown).
// Capture it before showing a window that will take focus, so whatever the
// user was working in can be restored later. See frontmostAppPidC.
func FrontmostAppPid() int {
	return int(C.frontmostAppPidC())
}

// ActivateAppByPid brings the given app back to the front. A no-op for a
// pid that is 0 or no longer running.
func ActivateAppByPid(pid int) {
	C.activateAppByPidC(C.int(pid))
}

// describeWindow returns the window's on-screen frame and visibility.
func describeWindow(window unsafe.Pointer) (x, y, w, h float64, visible bool) {
	var out [5]C.double
	C.describeWindowC(window, &out[0])
	return float64(out[0]), float64(out[1]), float64(out[2]), float64(out[3]), out[4] != 0
}

//export goMainThreadTrampoline
func goMainThreadTrampoline(handle C.uintptr_t) {
	h := cgo.Handle(handle)
	f := h.Value().(func())
	h.Delete()
	f()
}
