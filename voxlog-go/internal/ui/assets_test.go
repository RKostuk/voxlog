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
	// Summarization is the other thing that model is for, so its controls
	// belong beside it rather than in Advanced, where a switch like this
	// drifts to.
	for _, id := range []string{`id="summary_enabled"`, `id="summary_length"`, `id="summary_prompt_extra"`, `id="summary_prompt_reset"`,
		`id="llm_provider"`, `id="llm_base_url"`, `id="llm_model"`, `id="llm_api_key"`, `id="llm-test-btn"`} {
		if !strings.Contains(llm, id) {
			t.Errorf("%s should live in the LLM pane", id)
		}
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

// The API key must not be part of the settings payload: it belongs in the
// keychain, and settings.json is a plain file that gets opened and synced.
func TestTheAPIKeyIsNeverPutInTheSettingsPayload(t *testing.T) {
	frag, err := assets.ReadFile("assets/settings-pane.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(frag)
	payload := body[strings.Index(body, "function buildPayload("):]
	payload = payload[:strings.Index(payload, "function save(")]
	if strings.Contains(payload, "llm_api_key") {
		t.Error("buildPayload carries the API key; it must go to setLLMAPIKey instead")
	}
	// The field is a password field, and what is typed into it is handed off
	// and cleared rather than kept in the window.
	if !strings.Contains(body, `type="password" id="llm_api_key"`) {
		t.Error("the API key field is not a password field")
	}
	if !strings.Contains(body, "window.setLLMAPIKey(") {
		t.Error("nothing hands the key to the keychain")
	}
}

// kit.js exists for the same reason kit.css does: the drawer used to carry
// its own copy of every shared helper, and they had drifted -- a project drew
// in one colour in the main window and another in the drawer, and one status
// had two names. A second copy anywhere is how that comes back.
func TestSharedHelpersLiveOnlyInTheKit(t *testing.T) {
	kit, err := assets.ReadFile("assets/kit.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range []string{"function escapeHtml(", "function entityColor(", "var taskStatusLabel =", "function taskStatusClass(", "function duration(", "function clockTime("} {
		if !strings.Contains(string(kit), decl) {
			t.Errorf("kit.js is missing %q", decl)
		}
	}

	for _, page := range []string{"main.html", "drawer.html", "settings-pane.html"} {
		body, err := assets.ReadFile("assets/" + page)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range []string{"function escapeHtml(", "function escapeHTML(", "function entityColor(", "function duration(", "function clockTime(", "var taskStatusLabel", "var STATUS_LABEL"} {
			if strings.Contains(string(body), decl) {
				t.Errorf("%s declares its own %q instead of using kit.js", page, decl)
			}
		}
	}
}

// Both windows are spliced from the same parts. A page that misses the splice
// loads with a bare marker where its tokens or helpers should be.
func TestBothWindowsCarryTheKitMarkers(t *testing.T) {
	for _, page := range []string{"main.html", "drawer.html"} {
		body, err := assets.ReadFile("assets/" + page)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{kitCSSMarker, kitJSMarker} {
			if !strings.Contains(string(body), marker) {
				t.Errorf("%s has no %s marker, so it would render without the shared kit", page, marker)
			}
		}
	}
}

// The scales are the point of the exercise: without them the next padding is
// written out by hand, like the thirteen before it.
func TestKitDefinesTheScales(t *testing.T) {
	css, err := assets.ReadFile("assets/kit.css")
	if err != nil {
		t.Fatal(err)
	}
	root, _, ok := strings.Cut(string(css), "@media")
	if !ok {
		t.Fatal("kit.css has no dark-mode block at all")
	}
	for _, token := range []string{
		"--sp-4:", "--r-md:", "--r-pill:", "--fs-caption:", "--fs-body:", "--fw-bold:",
		"--glass:", "--purple:", "--green-ink:", "--red-ink:", "--yellow-ink:",
	} {
		if !strings.Contains(root, token) {
			t.Errorf("%s is not defined on bare :root", token)
		}
	}
}

// Ink colours sit on a tinted chip at text size, so they have to change with
// the theme. Hardcoded #1b8a3e/#c62b22 did not, which is what this guards.
func TestInkColoursAreRedefinedForDark(t *testing.T) {
	css, err := assets.ReadFile("assets/kit.css")
	if err != nil {
		t.Fatal(err)
	}
	_, dark, ok := strings.Cut(string(css), "@media")
	if !ok {
		t.Fatal("kit.css has no dark-mode block at all")
	}
	for _, token := range []string{"--green-ink:", "--red-ink:", "--yellow-ink:", "--purple:"} {
		if !strings.Contains(dark, token) {
			t.Errorf("%s is never redefined for the dark theme", token)
		}
	}
	for _, literal := range []string{"#1b8a3e", "#c62b22", "#92700a"} {
		page, err := assets.ReadFile("assets/main.html")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(page), literal) {
			t.Errorf("main.html still hardcodes %s instead of using the ink token", literal)
		}
	}
}

// Overview's weight: the chart is drawn straight onto the pane (a card around
// eight short columns reads as an empty band), and Recent takes ten rows from
// three sources rather than three rows from two.
func TestOverviewGivesRecentTheWeight(t *testing.T) {
	page, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	if strings.Contains(body, `class="card chart"`) {
		t.Error("the chart is back inside a card")
	}
	if !strings.Contains(body, "overviewRecentRows = 10") {
		t.Error("Recent is no longer ten rows")
	}
	if !strings.Contains(body, `id="overview-span"`) {
		t.Error("the Today / All time toggle is gone")
	}
	if !strings.Contains(body, "function taskRecentRowHTML(") {
		t.Error("tasks no longer reach Recent")
	}
}
