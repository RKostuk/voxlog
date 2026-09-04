package ui

import (
	"strings"
	"testing"
)

func TestInjectAssetSplicesTheFileAtItsMarker(t *testing.T) {
	page := "a\n  /* HERE */\nb"
	got := injectAsset(page, "  /* HERE */", "indicator.css")
	if got == page {
		t.Fatal("the marker was left in place -- nothing was spliced")
	}
	if !strings.Contains(got, "a\n") || !strings.Contains(got, "\nb") {
		t.Fatalf("the surrounding page was damaged: %q", got)
	}
}

// A renamed or deleted asset must leave a page that still loads. The miss is
// then obvious on screen, which is a far better failure than a blank window.
func TestInjectAssetLeavesThePageAloneWhenTheFileIsGone(t *testing.T) {
	page := "a\n  /* HERE */\nb"
	if got := injectAsset(page, "  /* HERE */", "nope.css"); got != page {
		t.Fatalf("got %q, want the page unchanged", got)
	}
}

// The kit is the one place the palette is defined. A token defined only
// inside a media query renders one theme's text on the other theme's
// background, which is the classic unreadable-window bug.
func TestKitDefinesEveryColourTokenOnBareRoot(t *testing.T) {
	css, err := assets.ReadFile("assets/kit.css")
	if err != nil {
		t.Fatal(err)
	}
	root, _, ok := strings.Cut(string(css), "@media")
	if !ok {
		t.Fatal("kit.css has no dark-mode block at all")
	}
	for _, token := range []string{
		"--bg:", "--card:", "--sidebar:", "--separator:", "--label:",
		"--secondary:", "--tertiary:", "--field:", "--accent:",
		"--green:", "--yellow:", "--red:",
	} {
		if !strings.Contains(root, token) {
			t.Errorf("%s is not defined on bare :root", token)
		}
	}
}

// The shell and the settings fragment live in one document. If the fragment
// still used data-pane, one click would drive both navs.
func TestSettingsFragmentDoesNotFightTheShellNav(t *testing.T) {
	frag, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(frag)
	if strings.Contains(body, "data-pane=") {
		t.Error("the fragment still uses data-pane; it must use data-subpane")
	}
	for _, tag := range []string{"<html", "<head", "<body", "<style"} {
		if strings.Contains(body, tag) {
			t.Errorf("%s has no place in a fragment", tag)
		}
	}
	if !strings.Contains(body, `id="settings-pane"`) {
		t.Error("the fragment needs a single root element with id settings-pane")
	}
	for _, subpane := range []string{"dictation", "model", "audio", "meetings", "history", "advanced"} {
		if !strings.Contains(body, `data-subpane="`+subpane+`"`) {
			t.Errorf("the %s category did not survive the move", subpane)
		}
	}
}
