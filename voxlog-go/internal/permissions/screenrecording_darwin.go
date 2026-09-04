package permissions

/*
#cgo LDFLAGS: -framework CoreGraphics
#include <CoreGraphics/CoreGraphics.h>
*/
import "C"

// ScreenRecording reports whether this process may capture the screen --
// the permission macOS also requires for audio-only ScreenCaptureKit
// capture, which is how system audio recording works here.
func ScreenRecording() bool {
	return bool(C.CGPreflightScreenCaptureAccess())
}

// RequestScreenRecording asks macOS to show its Screen Recording prompt if
// the permission hasn't been decided yet. Like Accessibility, a fresh grant
// only takes effect after the app restarts, so callers should say so rather
// than waiting for ScreenRecording() to flip.
func RequestScreenRecording() bool {
	return bool(C.CGRequestScreenCaptureAccess())
}
