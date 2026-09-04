package ui

import (
	"regexp"
	"strings"
	"testing"
)

func TestSetPositionKeepsEveryOfferedValue(t *testing.T) {
	for _, want := range OverlayPositions {
		o := &Overlay{}
		o.SetPosition(want)
		if o.position != want {
			t.Errorf("SetPosition(%q) stored %q", want, o.position)
		}
	}
}

func TestSetPositionFallsBackToCursor(t *testing.T) {
	// A settings file from a newer build, or one edited by hand, must not be
	// able to leave the overlay with a placement nothing knows how to apply.
	for _, raw := range []string{"", "middle", "TOP_LEFT", "top-left"} {
		o := &Overlay{position: "top_right"}
		o.SetPosition(raw)
		if o.position != OverlayAtCursor {
			t.Errorf("SetPosition(%q) stored %q, want %q", raw, o.position, OverlayAtCursor)
		}
	}
}

func TestOverlayCornersAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for name, corner := range overlayCorners {
		if corner < 0 || corner > 5 {
			t.Errorf("%s maps to corner %d, outside positionInCorner's 0..5", name, corner)
		}
		if other, dup := seen[corner]; dup {
			t.Errorf("%s and %s both map to corner %d", name, other, corner)
		}
		seen[corner] = name
	}
	if len(seen) != len(overlayCorners) {
		t.Errorf("got %d distinct corners for %d names", len(seen), len(overlayCorners))
	}
}

// The settings window is plain HTML with no compiler between it and this
// package, so a value can be offered in the picker that the Go side silently
// ignores (falling back to the cursor), or a supported placement can quietly
// stop being offered. Comparing the two lists is the only thing that notices.
func TestSettingsPickerOffersExactlyTheSupportedPositions(t *testing.T) {
	html, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}

	// The picker is a map of the screen now, not a list of names: each spot
	// carries the value it sets.
	offered := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-pos="([^"]+)"`).
		FindAllStringSubmatch(string(html), -1) {
		offered[m[1]] = true
	}

	supported := map[string]bool{}
	for _, p := range OverlayPositions {
		supported[p] = true
		if !offered[p] {
			t.Errorf("%q is supported but the settings picker does not offer it", p)
		}
	}
	for value := range offered {
		if !supported[value] {
			t.Errorf("the settings picker offers %q, which SetPosition would discard", value)
		}
	}
}

// Switching models must not rewrite settings the newly selected model does
// not happen to use. Selecting a model without language support used to
// force the language back to "auto" and switching away left that behind, so
// a chosen Ukrainian silently became auto-detect.
func TestSettingsWindowDoesNotResetUnsupportedOptions(t *testing.T) {
	html, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		`if (!langOK) document.getElementById('language').value = 'auto';`,
		`if (!streamOK) document.getElementById('live_streaming_text').checked = false;`,
	} {
		if strings.Contains(string(html), forbidden) {
			t.Errorf("settings-pane.html still clobbers a setting on model switch:\n  %s", forbidden)
		}
	}
}

func TestStyleOrDefaultFallsBackToCuff(t *testing.T) {
	// A settings file from a newer build must not be able to leave the
	// overlay with a style nothing knows how to size or place.
	for _, raw := range []string{"", "notch", "CUFF", "teleprompter"} {
		name, style := styleOrDefault(raw)
		if name != DefaultIndicatorStyle {
			t.Errorf("styleOrDefault(%q) = %q, want %q", raw, name, DefaultIndicatorStyle)
		}
		if style.Width == 0 || style.Height == 0 {
			t.Errorf("styleOrDefault(%q) returned an unsizable style %+v", raw, style)
		}
	}
}

func TestEveryIndicatorStyleIsPlaceableAndSized(t *testing.T) {
	placeable := map[string]bool{}
	for _, p := range OverlayPositions {
		placeable[p] = true
	}
	for name, style := range IndicatorStyles {
		// A style's default has to be a placement place() can actually apply,
		// or choosing that style puts the indicator somewhere undefined.
		if !placeable[style.DefaultPosition] {
			t.Errorf("%s defaults to position %q, which place() does not handle",
				name, style.DefaultPosition)
		}
		if style.Width <= 0 || style.Height <= 0 {
			t.Errorf("%s has no usable size: %+v", name, style)
		}
		// A style that grows for live text must grow, not shrink: the window
		// keeps its bottom-left origin on resize, so a narrower frame would
		// leave the text clipped rather than wrapped.
		if style.StreamingWidth != 0 && style.StreamingWidth <= style.Width {
			t.Errorf("%s streams at %.0f, no wider than its %.0f resting width",
				name, style.StreamingWidth, style.Width)
		}
	}
	if _, ok := IndicatorStyles[DefaultIndicatorStyle]; !ok {
		t.Fatalf("the default style %q is not in IndicatorStyles", DefaultIndicatorStyle)
	}
}

func TestIndicatorPartsRenderAsClasses(t *testing.T) {
	all := IndicatorParts{Wave: true, Timer: true, ModeLabel: true,
		StopButton: true, Solid: true, Outline: true}
	got := all.classList("capsule", OverlayTopCentre)
	for _, want := range []string{
		"style-capsule", "at-top_centre", "on-wave", "on-timer", "on-mode", "on-stop", "on-solid", "on-outline",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("classList = %q, missing %q", got, want)
		}
	}

	// Nothing switched on must produce the style and nothing else, or the
	// page would keep drawing parts the user turned off.
	if got := (IndicatorParts{}).classList("capsule", OverlayAtCursor); got != "style-capsule at-cursor" {
		t.Errorf("empty parts rendered %q, want %q", got, "style-capsule at-cursor")
	}
}

// The settings window is plain HTML with no compiler between it and this
// package. A style offered there that Go does not know falls silently back
// to the default; a style Go supports but the picker omits is unreachable.
func TestSettingsPickerOffersExactlyTheSupportedStyles(t *testing.T) {
	html, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(html), "var INDICATOR_STYLES = [")
	if start < 0 {
		t.Fatal("no INDICATOR_STYLES list in settings-pane.html")
	}
	end := strings.Index(string(html)[start:], "];")
	if end < 0 {
		t.Fatal("unterminated INDICATOR_STYLES list")
	}
	block := string(html)[start : start+end]

	offered := map[string]bool{}
	for _, m := range regexp.MustCompile(`\['([a-z]+)',`).FindAllStringSubmatch(block, -1) {
		offered[m[1]] = true
	}
	for name := range IndicatorStyles {
		if !offered[name] {
			t.Errorf("%q is a supported style but the settings picker does not offer it", name)
		}
	}
	for value := range offered {
		if _, ok := IndicatorStyles[value]; !ok {
			t.Errorf("the settings picker offers %q, which Configure would discard", value)
		}
	}
}

// Settings previews the indicator by building the same class list Go does.
// If the two disagree the preview lies about what you are choosing, which is
// worse than showing nothing -- so the option-to-class mapping is compared
// directly rather than trusted.
func TestSettingsPreviewBuildsTheSameClassesAsGo(t *testing.T) {
	html, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)

	start := strings.Index(page, "function indicatorClasses(")
	if start < 0 {
		t.Fatal("settings-pane.html no longer builds a class list for the preview")
	}
	block := page[start : start+strings.Index(page[start:], "\n    }")]

	all := IndicatorParts{Wave: true, Timer: true, ModeLabel: true,
		StopButton: true, Solid: true, Outline: true}
	for _, class := range strings.Fields(all.classList("cuff", OverlayTopCentre)) {
		if strings.HasPrefix(class, "style-") || strings.HasPrefix(class, "at-") {
			continue // built from the chosen style and placement, not listed
		}
		if !strings.Contains(block, "'"+class+"'") {
			t.Errorf("Go emits %q but the Settings preview never applies it", class)
		}
	}
}

// The preview and the overlay must be drawn by the same stylesheet. Both
// pages carry a marker Go splices assets/indicator.css into; losing either
// one leaves a page with markup and no component styling at all. The preview
// lives in the Settings pane, and the page that hosts that pane is the one
// that owns the marker.
func TestBothPagesTakeTheSharedIndicatorCSS(t *testing.T) {
	css, err := assets.ReadFile("assets/indicator.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"assets/overlay.html", "assets/main.html"} {
		page, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(page), indicatorCSSMarker) {
			t.Errorf("%s has no %s marker, so it gets no indicator styling", name, indicatorCSSMarker)
			continue
		}
		if injected := injectAsset(string(page), indicatorCSSMarker, "indicator.css"); !strings.Contains(injected, ".ind {") {
			t.Errorf("%s did not take the shared CSS", name)
		}
	}

	// The component may not name an element: the classes land on <body> in
	// the overlay and on a wrapper div in Settings.
	if strings.Contains(string(css), "body.") || strings.Contains(string(css), "body ") {
		t.Error("indicator.css names <body>, so it cannot style the Settings preview")
	}
}

// Go builds the class list; the stylesheet is the only thing that reads it.
// If a class stops being styled the option keeps saving and stops doing
// anything, which is the failure mode nobody notices. Checked against the
// page as Go serves it, since the component is spliced in at load.
func TestOverlayPageStylesEveryClassGoEmits(t *testing.T) {
	raw, err := assets.ReadFile("assets/overlay.html")
	if err != nil {
		t.Fatal(err)
	}
	page := []byte(injectAsset(string(raw), indicatorCSSMarker, "indicator.css"))
	all := IndicatorParts{Wave: true, Timer: true, ModeLabel: true,
		StopButton: true, Solid: true, Outline: true}
	for name, style := range IndicatorStyles {
		for _, class := range strings.Fields(all.classList(name, style.DefaultPosition)) {
			if !strings.Contains(string(page), "."+class) {
				t.Errorf("overlay.html never styles %q", class)
			}
		}
	}
}

// Restructuring settings-pane.html moves large blocks of markup around, and the
// only thing holding the page together is that every id the script reaches
// for still exists exactly once. Nothing else notices a dropped one: the
// page throws on load, the form never fills, and Settings opens blank.
func TestSettingsPageHasEveryIDItsScriptReads(t *testing.T) {
	html, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)

	declared := map[string]int{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		declared[m[1]]++
	}
	for id, n := range declared {
		if n > 1 {
			t.Errorf("id %q is declared %d times; getElementById would find only the first", id, n)
		}
	}

	// Ids built at runtime carry a model family or variant in them, so they
	// cannot be checked against static markup.
	dynamic := regexp.MustCompile(`^(prog|dl)-`)
	for _, m := range regexp.MustCompile(`getElementById\('([^']+)'\)`).FindAllStringSubmatch(page, -1) {
		id := m[1]
		if dynamic.MatchString(id) {
			continue
		}
		if declared[id] == 0 {
			t.Errorf("the script reads #%s, which the markup does not declare", id)
		}
	}
}

// The sidebar and the panes are two lists that have to agree. A nav item
// with no pane selects nothing; a pane with no nav item is unreachable and
// its settings can never be changed again.
func TestSettingsSidebarAndPanesMatch(t *testing.T) {
	html, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)

	navs := map[string]bool{}
	for _, m := range regexp.MustCompile(`class="subnav-item"[^>]*data-subpane="([^"]+)"`).
		FindAllStringSubmatch(page, -1) {
		navs[m[1]] = true
	}
	panes := map[string]bool{}
	for _, m := range regexp.MustCompile(`class="subpane(?: on)?" data-subpane="([^"]+)"`).
		FindAllStringSubmatch(page, -1) {
		panes[m[1]] = true
	}

	if len(navs) == 0 || len(panes) == 0 {
		t.Fatalf("found %d nav items and %d panes; the selectors have drifted", len(navs), len(panes))
	}
	for name := range navs {
		if !panes[name] {
			t.Errorf("sidebar offers %q, which has no pane", name)
		}
	}
	for name := range panes {
		if !navs[name] {
			t.Errorf("pane %q has no sidebar entry, so it can never be opened", name)
		}
	}

	// Exactly one pane may start visible, or the window opens showing two.
	if n := strings.Count(page, `class="subpane on"`); n != 1 {
		t.Errorf("%d panes start open, want exactly 1", n)
	}
}

// The placement map and the preview are two halves of one control: a spot
// you can click but that the preview cannot draw leaves you choosing
// blind, which is the exact problem the map was added to fix. The rule that
// draws it lives in settings.css now, alongside the rest of the pane's
// styling.
func TestSettingsPreviewDrawsEveryOfferedPosition(t *testing.T) {
	raw, err := assets.ReadFile("assets/settings.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	for _, pos := range OverlayPositions {
		if !strings.Contains(css, ".preview-mount.at-"+pos) {
			t.Errorf("position %q can be chosen but the preview has no .at-%s rule", pos, pos)
		}
	}
}

// Every style's default placement has to be one the Settings map can show as
// selected, or picking that style leaves no spot lit.
func TestSettingsKnowsEveryStyleDefaultPosition(t *testing.T) {
	html, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(html), "var STYLE_DEFAULT_POSITION = {")
	if start < 0 {
		t.Fatal("settings-pane.html no longer maps styles to a default position")
	}
	block := string(html)[start : start+strings.Index(string(html)[start:], "};")]

	for name, style := range IndicatorStyles {
		if !strings.Contains(block, name+":") {
			t.Errorf("style %q has no default position in Settings", name)
			continue
		}
		if !strings.Contains(block, "'"+style.DefaultPosition+"'") {
			t.Errorf("Go defaults %s to %q, which Settings never uses",
				name, style.DefaultPosition)
		}
	}
}

// The cuff used to be its own style. It is now what the capsule does when
// it is placed against the menu bar, which only works if the page is told
// the placement -- and only matters if the stylesheet acts on it.
func TestCapsuleSquaresItsShouldersAtTheNotch(t *testing.T) {
	classes := IndicatorParts{Wave: true}.classList("capsule", OverlayTopCentre)
	if !strings.Contains(classes, "at-"+OverlayTopCentre) {
		t.Fatalf("classList = %q, carries no placement for the page to react to", classes)
	}

	css, err := assets.ReadFile("assets/indicator.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".at-"+OverlayTopCentre+".style-capsule") {
		t.Error("nothing reshapes the capsule at the top centre, so it never meets the menu bar")
	}
}

// A hand-edited settings file, or one written before the sliders existed,
// must not be able to produce a meter wider than the window it lives in --
// and zero has to mean "the default", not "invisible".
func TestClampWaveKeepsTheMeterInsideItsWindow(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, waveDefWidth},
		{-20, waveMinWidth},
		{10, waveMinWidth},
		{9000, waveMaxWidth},
		{120, 120},
	}
	for _, c := range cases {
		if got := clampWave(c.in, waveMinWidth, waveMaxWidth, waveDefWidth); got != c.want {
			t.Errorf("clampWave(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
