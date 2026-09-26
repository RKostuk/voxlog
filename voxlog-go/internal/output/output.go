// Package output places transcribed text on the clipboard and, optionally,
// simulates a Cmd+V paste. Port of dictate_app/paste.py.
package output

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework ApplicationServices -framework Cocoa
#include <ApplicationServices/ApplicationServices.h>
#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include <string.h>

// The clipboard is read and written through NSPasteboard rather than by
// shelling out to pbcopy/pbpaste. pbcopy has no encoding argument: it decodes
// whatever arrives on stdin using the locale from the environment, and an app
// launched from Finder inherits no LANG at all -- so it fell back to MacRoman
// and turned every Cyrillic transcript into mojibake ("Привіт" pasted as
// "–ü—Ä–∏–≤—ñ—Ç"). It only ever reproduced in the bundled app; running the same
// binary from a terminal inherited the shell's UTF-8 locale and looked fine.
// NSPasteboard takes an NSString, so there is no encoding to guess.
static void writeClipboard(const char *utf8) {
	@autoreleasepool {
		NSString *s = [NSString stringWithUTF8String:utf8];
		if (s == nil) {
			return;
		}
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		[pb clearContents];
		[pb setString:s forType:NSPasteboardTypeString];
	}
}

// readClipboard returns the pasteboard's text as a UTF-8 C string the caller
// must free, or NULL when it holds no text (an image, a file, or nothing).
static char *readClipboard(void) {
	@autoreleasepool {
		NSString *s = [[NSPasteboard generalPasteboard] stringForType:NSPasteboardTypeString];
		if (s == nil) {
			return NULL;
		}
		return strdup([s UTF8String]);
	}
}

// kVK_ANSI_V and kVK_Command are physical keycodes, independent of the
// active input source (layout-agnostic, unlike resolving by character).
static const CGKeyCode kVKAnsiV = 9;
static const CGKeyCode kVKCommand = 55;

// postCmdV posts a real 4-event sequence: Cmd-down, V-down (Cmd flag set),
// V-up (Cmd flag set), Cmd-up. Posting an actual Cmd key-down/up (not just
// the flag bit on the V events) makes this work for apps that check live
// modifier-key state via CGEventSourceKeyState, not just NSEvent.modifierFlags.
static void postCmdV(void) {
	CGEventRef cmdDown = CGEventCreateKeyboardEvent(NULL, kVKCommand, true);
	CGEventRef vDown = CGEventCreateKeyboardEvent(NULL, kVKAnsiV, true);
	CGEventRef vUp = CGEventCreateKeyboardEvent(NULL, kVKAnsiV, false);
	CGEventRef cmdUp = CGEventCreateKeyboardEvent(NULL, kVKCommand, false);

	CGEventSetFlags(vDown, kCGEventFlagMaskCommand);
	CGEventSetFlags(vUp, kCGEventFlagMaskCommand);

	CGEventPost(kCGHIDEventTap, cmdDown);
	CGEventPost(kCGHIDEventTap, vDown);
	CGEventPost(kCGHIDEventTap, vUp);
	CGEventPost(kCGHIDEventTap, cmdUp);

	CFRelease(cmdDown);
	CFRelease(vDown);
	CFRelease(vUp);
	CFRelease(cmdUp);
}
*/
import "C"

import (
	"time"
	"unsafe"
)

// ReadClipboard returns the pasteboard's current text, and false if it holds
// no text at all.
func ReadClipboard() (string, bool) {
	c := C.readClipboard()
	if c == nil {
		return "", false
	}
	defer C.free(unsafe.Pointer(c))
	return C.GoString(c), true
}

// WriteClipboard replaces the pasteboard's contents with text.
func WriteClipboard(text string) {
	c := C.CString(text)
	defer C.free(unsafe.Pointer(c))
	C.writeClipboard(c)
}

// pasteSettleDelay mirrors paste.py's _PASTE_SETTLE_SECONDS: how long to
// wait after simulating Cmd+V before restoring the previous clipboard
// contents, so the target app finishes reading the pasteboard first.
const pasteSettleDelay = 150 * time.Millisecond

// Output modes, as stored in Settings.OutputMode.
const (
	// ModeNone records the transcript to history only -- the clipboard and
	// the focused app are left completely untouched.
	ModeNone = "none"
	// ModeCopy leaves the text on the clipboard.
	ModeCopy = "copy"
	// ModePaste types the text into the focused app, then restores the
	// clipboard to whatever it held before, so dictation doesn't clobber
	// what the user had copied.
	ModePaste = "paste"
	// ModePasteCopy types the text into the focused app AND leaves it on
	// the clipboard afterward (no restore) -- for when the transcript is
	// wanted in both places.
	ModePasteCopy = "paste_copy"
	// ModePasteUnlessTask pastes like ModePaste, except a dictation the LLM
	// reads as a task is not pasted at all -- it was said to be written down,
	// not typed. Emit itself just pastes: whether to call it is the caller's
	// decision, made on the classifier's answer (see recordDictationResult).
	ModePasteUnlessTask = "paste_unless_task"
)

// Emit delivers text according to mode. If text is empty it does nothing
// (matching paste.py, which no-ops on empty text rather than blanking the
// clipboard or firing a real Cmd+V into whatever app has focus).
func Emit(text string, mode string) error {
	if text == "" || mode == ModeNone {
		return nil
	}

	switch mode {
	case ModePaste, ModePasteUnlessTask:
		// A pasteboard holding an image or a file has no text to put back, so
		// hadText decides whether the restore happens at all -- writing an
		// empty string instead would silently clear whatever was there.
		previous, hadText := ReadClipboard()
		WriteClipboard(text)
		C.postCmdV()
		time.Sleep(pasteSettleDelay)
		if hadText {
			WriteClipboard(previous)
		}
		return nil

	case ModePasteCopy:
		WriteClipboard(text)
		C.postCmdV()
		// Settle before returning (not before a restore, since there isn't
		// one here) so the target app has finished reading the pasteboard
		// before anything else can overwrite it.
		time.Sleep(pasteSettleDelay)
		return nil

	default: // ModeCopy
		WriteClipboard(text)
		return nil
	}
}
