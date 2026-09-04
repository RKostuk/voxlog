package permissions

import "voxlog-go/internal/hotkey"

// Accessibility reports whether this process currently has Accessibility
// permission (needed for the global dictate/history hotkeys), without
// prompting.
func Accessibility() bool {
	return hotkey.IsAccessibilityTrusted()
}

// RequestAccessibility shows macOS's native Accessibility grant dialog if
// not already trusted. The OS only picks up a fresh grant after the
// process restarts, so the caller should tell the user to relaunch Voxlog
// rather than expecting Accessibility() to flip true in this same run.
func RequestAccessibility() bool {
	return hotkey.PromptAccessibilityTrust()
}
