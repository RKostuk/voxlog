package hotkey

/*
#cgo LDFLAGS: -framework ApplicationServices
#include <ApplicationServices/ApplicationServices.h>

// AXIsProcessTrustedWithOptions wants a CFDictionary with the
// kAXTrustedCheckOptionPrompt key set to kCFBooleanTrue to make macOS show
// its own native "Voxlog wants to control this computer using accessibility
// features" dialog (with a button straight to System Settings) instead of
// just silently returning false. Building that one-entry dictionary is
// easier in C than via cgo's CFDictionary bindings.
static Boolean checkAccessibilityTrustedC(Boolean prompt) {
	CFStringRef key = kAXTrustedCheckOptionPrompt;
	CFTypeRef value = prompt ? kCFBooleanTrue : kCFBooleanFalse;
	CFDictionaryRef options = CFDictionaryCreate(
		kCFAllocatorDefault, (const void **)&key, (const void **)&value, 1,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	Boolean trusted = AXIsProcessTrustedWithOptions(options);
	CFRelease(options);
	return trusted;
}
*/
import "C"

// IsAccessibilityTrusted reports whether this process currently has
// Accessibility permission, without prompting.
func IsAccessibilityTrusted() bool {
	return C.checkAccessibilityTrustedC(C.Boolean(0)) != 0
}

// PromptAccessibilityTrust shows macOS's own native Accessibility
// permission dialog if this process isn't already trusted (a no-op, no
// dialog, if it already is). Returns the trust state at the time of the
// call -- immediately after a fresh grant the OS may still report false
// until the process restarts, so this is a hint, not a guarantee.
func PromptAccessibilityTrust() bool {
	return C.checkAccessibilityTrustedC(C.Boolean(1)) != 0
}
