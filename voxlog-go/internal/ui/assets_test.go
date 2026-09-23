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
	for _, subpane := range []string{"dictation", "model", "llm", "audio", "meetings", "listening", "tasks", "history", "mcp", "advanced"} {
		if !strings.Contains(body, `data-subpane="`+subpane+`"`) {
			t.Errorf("the %s category did not survive the move", subpane)
		}
	}
}

// The LLM pane answers one question -- which model, and is it on disk. The
// project dictionary, the rejected lines and the drawer are about tasks, and
// putting them back beside the model download is the mistake this guards.
func TestTaskSettingsLiveInTheTasksPane(t *testing.T) {
	frag, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(frag)
	tasks := body[strings.Index(body, `class="subpane" data-subpane="tasks"`):]
	tasks = tasks[:strings.Index(tasks, `class="subpane" data-subpane="history"`)]
	for _, id := range []string{`id="entity-dict-list"`, `id="rejected-list"`, `id="tasks_drawer_placement"`} {
		if !strings.Contains(tasks, id) {
			t.Errorf("%s should live in the Tasks pane", id)
		}
	}

	llm := body[strings.Index(body, `class="subpane" data-subpane="llm"`):]
	llm = llm[:strings.Index(llm, `class="subpane" data-subpane="audio"`)]
	if !strings.Contains(llm, `id="llm-model"`) {
		t.Error("the LLM pane still owns the model download")
	}
	for _, id := range []string{`id="entity-dict-list"`, `id="rejected-list"`} {
		if strings.Contains(llm, id) {
			t.Errorf("%s is about tasks, not about the model", id)
		}
	}
}

// The MCP pane is the one place the server is turned on, addressed and
// explained. Advanced is where a switch like this would drift to, and the
// slice below is what stops it: everything the pane owns has to be inside
// the pane.
func TestMCPSettingsLiveInTheirOwnPane(t *testing.T) {
	frag, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(frag)
	mcp := body[strings.Index(body, `class="subpane" data-subpane="mcp"`):]
	mcp = mcp[:strings.Index(mcp, `class="subpane" data-subpane="advanced"`)]
	for _, id := range []string{`id="mcp_enabled"`, `id="mcp_allow_write"`, `id="mcp-status"`, `id="mcp-url"`, `id="mcp-regen-token"`} {
		if !strings.Contains(mcp, id) {
			t.Errorf("%s should live in the MCP pane", id)
		}
	}

	advanced := body[strings.Index(body, `class="subpane" data-subpane="advanced"`):]
	for _, id := range []string{`id="mcp_enabled"`, `id="mcp-url"`} {
		if strings.Contains(advanced, id) {
			t.Errorf("%s is the MCP pane's, not Advanced's", id)
		}
	}
}

// A checkbox only reaches Go if it is in the payload the pane sends:
// saveSettings takes the whole settings struct, so a field left out of the
// payload is a field quietly set back to false on every save -- which is
// invisible until the day somebody notices their switch keeps turning
// itself off.
func assertSwitchRoundTrips(t *testing.T, body string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if !strings.Contains(body, key+": document.getElementById('"+key+"').checked") {
			t.Errorf("%s is never collected, so saving would clear it", key)
		}
		if !strings.Contains(body, "document.getElementById('"+key+"').checked = ") {
			t.Errorf("%s is never loaded back into the form", key)
		}
	}
}

func TestTheRecordingNoticeSwitchIsSavedAndLoaded(t *testing.T) {
	frag, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	assertSwitchRoundTrips(t, string(frag), "recording_notice")
}

func TestTheMCPSwitchesAreSavedAndLoaded(t *testing.T) {
	frag, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	assertSwitchRoundTrips(t, string(frag), "mcp_enabled", "mcp_allow_write")
}
