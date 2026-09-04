// Package permissions checks and requests the two macOS permissions Voxlog
// needs: Accessibility (for the global dictate/history hotkeys) and
// Microphone (for recording). Both are surfaced in the Settings window so
// the user can see what's missing and trigger the native grant dialog
// directly, instead of guessing why a hotkey silently does nothing.
package permissions

/*
#cgo LDFLAGS: -framework AVFoundation -framework Foundation
#include <objc/objc.h>
#include <objc/message.h>
#include <objc/runtime.h>

// AVAuthorizationStatus values (AVFoundation/AVCaptureDevice.h):
//   0 = NotDetermined, 1 = Restricted, 2 = Denied, 3 = Authorized
static long microphoneAuthStatusC(void) {
	// AVMediaTypeAudio's runtime value is the four-char string "soun" as an
	// NSString constant; building it from the literal avoids linking against
	// the AVMediaTypeAudio symbol directly (its exact linkage varies by SDK).
	id mediaType = ((id (*)(id, SEL, const char *))objc_msgSend)(
		(id)objc_getClass("NSString"), sel_registerName("stringWithUTF8String:"), "soun");
	return ((long (*)(id, SEL, id))objc_msgSend)(
		(id)objc_getClass("AVCaptureDevice"), sel_registerName("authorizationStatusForMediaType:"), mediaType);
}
*/
import "C"

// MicrophoneStatus is the current, cached OS authorization state -- it
// never itself prompts the user.
type MicrophoneStatus string

const (
	MicNotDetermined MicrophoneStatus = "not_determined"
	MicRestricted    MicrophoneStatus = "restricted"
	MicDenied        MicrophoneStatus = "denied"
	MicAuthorized    MicrophoneStatus = "authorized"
)

func Microphone() MicrophoneStatus {
	switch int(C.microphoneAuthStatusC()) {
	case 1:
		return MicRestricted
	case 2:
		return MicDenied
	case 3:
		return MicAuthorized
	default:
		return MicNotDetermined
	}
}
