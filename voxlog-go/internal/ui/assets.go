package ui

import (
	"embed"
	"strings"
)

//go:embed assets/*.html assets/*.css assets/*.js
var assets embed.FS

// Markers are the lines a page carries where a shared file belongs. The
// indicator is drawn twice -- for real in the overlay, and as a preview in
// Settings -- and a preview that has drifted from the thing it previews is
// worse than no preview, so both get the same bytes at load.
const (
	kitCSSMarker       = "  /* KIT_CSS */"
	kitJSMarker        = "  /* KIT_JS */"
	settingsCSSMarker  = "  /* SETTINGS_CSS */"
	indicatorCSSMarker = "  /* INDICATOR_CSS */"
	settingsMarkup     = "  <!-- SETTINGS_PANE -->"
)

// injectAsset splices assets/<name> into a page at its marker. A missing file
// leaves the marker where it is rather than the raw error: the page still
// loads, and the miss shows up on screen instead of as a blank window.
func injectAsset(page, marker, name string) string {
	body, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return page
	}
	return strings.Replace(page, marker, string(body), 1)
}

// MenuBarIcon is the status item's glyph, as a template image: black on
// transparent, which is what tells macOS to recolor it for the current menu
// bar appearance instead of drawing the bitmap as-is. Regenerate it from
// packaging/menubar.svg with `make icons`.
//
//go:embed assets/menubar.png
var MenuBarIcon []byte

// MenuBarIconRecording and MenuBarIconTranscribing are the same waveform in
// red and blue, shown while a take is being recorded and while it is being
// decoded. Unlike MenuBarIcon these are installed as regular (non-template)
// images, because a template image is drawn from its alpha alone -- macOS
// would throw the color away and paint them the same as the idle glyph.
//
//go:embed assets/menubar-recording.png
var MenuBarIconRecording []byte

//go:embed assets/menubar-transcribing.png
var MenuBarIconTranscribing []byte
