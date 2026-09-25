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
	for _, decl := range []string{"function escapeHtml(", "function entityColor(", "function entityOptionsHTML(", "var taskStatusLabel =", "function taskStatusClass(", "function duration(", "function clockTime("} {
		if !strings.Contains(string(kit), decl) {
			t.Errorf("kit.js is missing %q", decl)
		}
	}

	for _, page := range []string{"main.html", "drawer.html", "settings-pane.html"} {
		body, err := assets.ReadFile("assets/" + page)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range []string{"function escapeHtml(", "function escapeHTML(", "function entityColor(", "function entityOptionsHTML(", "function duration(", "function clockTime(", "var taskStatusLabel", "var STATUS_LABEL"} {
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

// openVoices takes a speaker row. Wiring it straight to a click listener
// handed it the MouseEvent instead, which went on to a Go binding expecting an
// int64 -- "json: cannot unmarshal object into Go value of type int64" -- and
// left the pane on "Loading voices…" forever. That was §14.
func TestVoicesIsNotOpenedWithAnEvent(t *testing.T) {
	page, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	if strings.Contains(body, "addEventListener('click', openVoices)") {
		t.Error("openVoices is wired as a listener, so its row argument is a MouseEvent")
	}
	if !strings.Contains(body, "namingRow = typeof row === 'number'") {
		t.Error("openVoices no longer checks that its row is a number")
	}
}

// webview_go installs no WKUIDelegate, so alert, confirm and prompt are all
// no-ops in this window: confirm answers false and prompt answers null
// without ever showing anything. That is why the Voices pane's Rename,
// Forget and Erase buttons looked dead -- every one of them was gated on a
// dialog that never appeared.
func TestTheVoicesPaneAsksForNothingThroughANativeDialog(t *testing.T) {
	page, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	pane := string(page)
	start := strings.Index(pane, "// ---- voices ---")
	end := strings.Index(pane, "// renderTasks draws the Tasks pane")
	if start < 0 || end < 0 || end < start {
		t.Fatal("cannot find the voices pane in main.html")
	}
	for _, call := range []string{"window.prompt(", "window.confirm(", "alert("} {
		if strings.Contains(pane[start:end], call) {
			t.Errorf("the voices pane still calls %s, which this webview never shows", call)
		}
	}
	// ...and the thing it uses instead has to be there.
	kit, err := assets.ReadFile("assets/kit.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kit), "function toast(") {
		t.Error("kit.js has no toast for the pane to report with")
	}
}

// Every list that can grow without a ceiling caps itself: a meeting the
// diarizer heard eight people in, and a voice library of dozens, were drawn
// in full and buried the panel they sat in.
func TestLongVoiceListsAreCapped(t *testing.T) {
	page, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"VOICE_PAGE = 5", "speakersShowAll", "data-show-all", "show-all-speakers"} {
		if !strings.Contains(string(page), needle) {
			t.Errorf("main.html is missing %q", needle)
		}
	}
}

// The drawer's composer is the only way to write a task down by hand, and it
// is three steps deep now: text, details, project. The ids are what the page
// wires its keyboard to, and what the hotkey's focusComposer reaches for --
// a renamed one leaves the drawer opening on nothing.
func TestDrawerCarriesTheStepwiseComposer(t *testing.T) {
	body, err := assets.ReadFile("assets/drawer.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, id := range []string{`id="draft-text"`, `id="draft-notes"`, `id="draft-entity"`, `id="draft-resume"`, `id="draft-step"`} {
		if !strings.Contains(page, id) {
			t.Errorf("drawer.html is missing %s", id)
		}
	}
	// Go evals this by name on every show of the window.
	if !strings.Contains(page, "window.focusComposer = focusComposer") {
		t.Error("drawer.html no longer exports focusComposer, so the hotkey would open a window with no caret in it")
	}
	// Three arguments, matching the drawerAddTask binding: a task written by
	// hand carries its notes and its project, not just a line of text.
	if !strings.Contains(page, "window.drawerAddTask(text, draft.notes.trim(), entity)") {
		t.Error("drawer.html no longer passes notes and project to drawerAddTask")
	}
	// The old single-line box, and the project coming from whichever chip
	// happened to be on, are what this replaces.
	if strings.Contains(page, `id="new-task"`) {
		t.Error("drawer.html still carries the one-line add box the composer replaced")
	}
}

// A task opens beside the list, not inside it: expanding a row in place
// pushed every task under it down the page, which is the thing this pane was
// asked to stop doing.
func TestTasksPaneOpensATaskBesideTheList(t *testing.T) {
	body, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, needle := range []string{`class="task-split"`, `id="task-panel"`, "function renderTaskDetail(", "function selectTask(",
		// The project and the reminder are set from the panel, not only read
		// off it: both used to be a line of grey text.
		"task-entity-pick", "task-reminder-at", "window.setTaskEntity(", "window.setTaskReminder("} {
		if !strings.Contains(page, needle) {
			t.Errorf("main.html is missing %s", needle)
		}
	}
	for _, gone := range []string{`class="task-detail"`, "openTaskIDs"} {
		if strings.Contains(page, gone) {
			t.Errorf("main.html still carries %s, the in-row detail this replaced", gone)
		}
	}
}

// Rejecting and deleting are the two irreversible things this pane can do.
// They used to sit on every row, revealed on hover, and fire on one click --
// a misclick on a list people scroll through. They belong to the open task,
// and they ask first.
func TestDestructiveTaskActionsAreConfirmedAndPanelOnly(t *testing.T) {
	body, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	if strings.Contains(page, `class="task-acts"`) {
		t.Error("the hover-revealed row actions are back on the list rows")
	}
	if !strings.Contains(page, `class="task-side-acts"`) {
		t.Error("the panel has lost its actions")
	}
	for _, needle := range []string{`data-confirm="Not a task`, `data-confirm="Delete`, "task-act-cancel", "classList.add('confirming')"} {
		if !strings.Contains(page, needle) {
			t.Errorf("main.html is missing %s -- the confirmation step", needle)
		}
	}
	// confirm() cannot be the confirmation: this webview has no
	// WKUIDelegate, so it answers false without asking.
	if strings.Contains(page, "confirm(") && !strings.Contains(page, "data-confirm") {
		t.Error("main.html calls confirm(), which is a no-op in this webview")
	}
}
