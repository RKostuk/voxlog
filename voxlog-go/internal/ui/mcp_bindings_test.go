package ui

import (
	"testing"

	"voxlog-go/internal/settings"
)

// Nothing in the app learned about a saved settings change before this seam
// existed -- always-on only gets away with it by polling the store every two
// seconds, and a switch that starts and stops a listener cannot poll. Driving
// a real save needs a window, so this covers the seam: the callback is
// installed, reachable, and handed the settings that were saved.
func TestSettingsAppliedFuncInstallsAReachableCallback(t *testing.T) {
	var got settings.Settings
	SetSettingsAppliedFunc(func(v settings.Settings) { got = v })
	t.Cleanup(func() { SetSettingsAppliedFunc(nil) })

	settingsApplied(settings.Settings{MCPEnabled: true, MCPPort: 51888})
	if !got.MCPEnabled || got.MCPPort != 51888 {
		t.Errorf("got %+v, want the settings passed through unchanged", got)
	}
}

// A window opened before startup finished has no callback, and that is not a
// fault -- it must not panic on the way past.
func TestSettingsAppliedWithNoCallbackIsHarmless(t *testing.T) {
	SetSettingsAppliedFunc(nil)
	settingsApplied(settings.Settings{})
}
