package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voxlog-go/internal/history"
	"voxlog-go/internal/output"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestGroupByDayNewestDayFirst(t *testing.T) {
	// AllEntries hands over oldest-first; the window shows newest first.
	groups := groupByDay([]history.Entry{
		{Timestamp: at(t, "2026-08-11T09:00:00Z"), Text: "monday"},
		{Timestamp: at(t, "2026-08-13T09:00:00Z"), Text: "wednesday"},
		{Timestamp: at(t, "2026-08-12T09:00:00Z"), Text: "tuesday"},
	}, nil)

	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	want := []string{"2026-08-13", "2026-08-12", "2026-08-11"}
	for i, day := range want {
		if groups[i].Day != day {
			t.Fatalf("group %d is %q, want %q", i, groups[i].Day, day)
		}
	}
}

func TestGroupByDayNewestEntryFirstWithinADay(t *testing.T) {
	groups := groupByDay([]history.Entry{
		{Timestamp: at(t, "2026-08-13T09:00:00Z"), Text: "first"},
		{Timestamp: at(t, "2026-08-13T10:00:00Z"), Text: "second"},
		{Timestamp: at(t, "2026-08-13T11:00:00Z"), Text: "third"},
	}, nil)

	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	got := groups[0].Entries
	if len(got) != 3 || got[0].Text != "third" || got[2].Text != "first" {
		t.Fatalf("got %+v, want newest-first within the day", got)
	}
}

func TestGroupByDayCarriesDisplayFields(t *testing.T) {
	groups := groupByDay([]history.Entry{{
		Timestamp:        at(t, "2026-08-13T14:05:00Z").Local(),
		Text:             "hello",
		DurationSeconds:  1.25,
		RecordingSeconds: 3.5,
	}}, nil)

	e := groups[0].Entries[0]
	if e.Text != "hello" || e.DurationSeconds != 1.25 || e.RecordingSeconds != 3.5 {
		t.Fatalf("got %+v", e)
	}
	if len(e.Time) != 5 || e.Time[2] != ':' {
		t.Fatalf("Time = %q, want HH:MM", e.Time)
	}
}

func TestGroupByDayEmptyInput(t *testing.T) {
	// The window JSON-encodes this directly; a nil slice would render as
	// "null" rather than an empty list.
	groups := groupByDay(nil, nil)
	if groups == nil {
		t.Fatal("want an empty non-nil slice")
	}
	if len(groups) != 0 {
		t.Fatalf("got %d groups, want 0", len(groups))
	}
}

func TestGroupByDaySplitsOnLocalDayBoundary(t *testing.T) {
	// Grouping uses the entry's own location, the same one the displayed
	// time comes from -- a day header must never disagree with the times
	// listed under it.
	loc := time.FixedZone("UTC+3", 3*60*60)
	groups := groupByDay([]history.Entry{
		{Timestamp: time.Date(2026, 8, 12, 23, 30, 0, 0, loc), Text: "late"},
		{Timestamp: time.Date(2026, 8, 13, 0, 30, 0, 0, loc), Text: "early"},
	}, nil)

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (23:30 and 00:30 are different days)", len(groups))
	}
	if groups[0].Day != "2026-08-13" || groups[0].Entries[0].Time != "00:30" {
		t.Fatalf("got %+v", groups[0])
	}
}

func TestMeetingsCarryWhatTheRowNeeds(t *testing.T) {
	at := time.Date(2026, 8, 18, 11, 0, 0, 0, time.Local)
	// HasAudio requires the file to actually stat, not just a non-empty path
	// (see TestMeetingWithNoAudioSaysSo) -- so this needs a real file on disk,
	// not the literal "/tmp/m-mic.wav" a stale record might still carry.
	audioPath := filepath.Join(t.TempDir(), "m-mic.wav")
	if err := os.WriteFile(audioPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := meetingsJSON([]history.Meeting{{
		Start:            at,
		RecordingSeconds: 2531,
		Text:             "a call",
		DurationSeconds:  9,
		AudioPath:        audioPath,
	}}, nil)

	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].ID != at.Format(time.RFC3339Nano) {
		t.Errorf("ID is %q; the Transcribe action passes it back to find the meeting again", got[0].ID)
	}
	if got[0].RecordingSeconds != 2531 {
		t.Errorf("got %v, want the length of the call", got[0].RecordingSeconds)
	}
	// Both numbers travel: the row's pill is the decode time, the line under
	// it the length of the call, and a 42-minute meeting decoded in 9 seconds
	// is precisely the case where showing one for the other misleads.
	if got[0].DurationSeconds != 9 {
		t.Errorf("got %v, want the decode time -- the row's pill shows it", got[0].DurationSeconds)
	}
	if !got[0].HasAudio {
		t.Error("HasAudio is false for a meeting whose audio path is set")
	}
}

// A meeting with both sides on disk must expose both -- otherwise the row's
// Play button can only ever play the mic track and the rest of the call is
// silent (see meetingJSON.SystemAudio).
func TestMeetingsCarryBothAudioTracks(t *testing.T) {
	dir := t.TempDir()
	micPath := filepath.Join(dir, "m-mic.wav")
	sysPath := filepath.Join(dir, "m-sys.wav")
	for _, p := range []string{micPath, sysPath} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := meetingsJSON([]history.Meeting{{
		Start:           time.Now(),
		AudioPath:       micPath,
		SystemAudioPath: sysPath,
	}}, nil)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Audio != "m-mic.wav" {
		t.Errorf("Audio = %q, want the mic track's name", got[0].Audio)
	}
	if got[0].SystemAudio != "m-sys.wav" {
		t.Errorf("SystemAudio = %q, want the system-audio track's name", got[0].SystemAudio)
	}
}

// A meeting recorded before the system-audio side was even a thing (or one
// whose call had nobody else on it worth capturing) has only the mic track --
// SystemAudio must be empty, not a stale or half-formed name.
func TestMeetingsWithOnlyMicAudioLeavesSystemAudioEmpty(t *testing.T) {
	dir := t.TempDir()
	micPath := filepath.Join(dir, "m-mic.wav")
	if err := os.WriteFile(micPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := meetingsJSON([]history.Meeting{{
		Start:     time.Now(),
		AudioPath: micPath,
		// SystemAudioPath left unset, or pointing at a file no longer there.
		SystemAudioPath: filepath.Join(dir, "gone.wav"),
	}}, nil)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].SystemAudio != "" {
		t.Errorf("SystemAudio = %q, want empty when that file is not on disk", got[0].SystemAudio)
	}
	if got[0].Audio == "" {
		t.Error("Audio should still be populated for the mic track that IS on disk")
	}
}

// The audio is gone once retention has swept it, and the row must stop
// offering to transcribe something that is not there.
func TestMeetingWithNoAudioSaysSo(t *testing.T) {
	got := meetingsJSON([]history.Meeting{{Start: time.Now(), RecordingSeconds: 60}}, nil)
	if len(got) != 1 || got[0].HasAudio {
		t.Fatalf("got %+v, want HasAudio false", got)
	}
}

// Dictations and meetings are separate stores now; nothing meeting-shaped may
// come back out of the day list.
func TestGroupByDayNoLongerCarriesAMeetingFlag(t *testing.T) {
	groups := groupByDay([]history.Entry{{
		Timestamp: time.Date(2026, 8, 18, 9, 0, 0, 0, time.Local),
		Text:      "a dictation",
	}}, nil)
	if len(groups) != 1 || len(groups[0].Entries) != 1 {
		t.Fatalf("got %+v, want one entry in one day", groups)
	}
}

// An un-migrated meeting sitting in a day file (failed migration, restored
// backup, or a sentinel copied ahead of the transcripts directory) must not
// render as a dictation row: it has no Transcribe button and its length
// shows wrong there. It belongs to the Meetings pane only.
func TestGroupByDaySkipsAnUnmigratedMeeting(t *testing.T) {
	groups := groupByDay([]history.Entry{
		{
			Timestamp: time.Date(2026, 8, 18, 9, 0, 0, 0, time.Local),
			Text:      "a dictation",
		},
		{
			Timestamp: time.Date(2026, 8, 18, 10, 0, 0, 0, time.Local),
			Kind:      history.KindMeeting,
			Text:      "a call left behind by migration",
		},
	}, nil)
	if len(groups) != 1 || len(groups[0].Entries) != 1 {
		t.Fatalf("got %+v, want the meeting excluded and only the dictation left", groups)
	}
	if groups[0].Entries[0].Text != "a dictation" {
		t.Fatalf("got %+v, want the surviving row to be the dictation", groups[0].Entries[0])
	}
}

// The page is served from loopback and asks for a recording by name, so the
// row carries the base name rather than the absolute path: nothing about the
// user's home directory needs to reach the page.
func TestRowsCarryOnlyTheRecordingsName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-08-18-142201-dictation.wav")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	groups := groupByDay([]history.Entry{{
		Timestamp: time.Date(2026, 8, 18, 14, 22, 1, 0, time.Local),
		Text:      "a dictation",
		AudioPath: path,
	}}, nil)
	if len(groups) != 1 || len(groups[0].Entries) != 1 {
		t.Fatalf("got %+v, want one entry", groups)
	}
	if got := groups[0].Entries[0].Audio; got != "2026-08-18-142201-dictation.wav" {
		t.Errorf("got %q, want just the file name", got)
	}
	if strings.Contains(groups[0].Entries[0].Audio, dir) {
		t.Error("the row leaked an absolute path to the page")
	}
}

// A recording that has been swept away must not leave a dead player behind.
func TestRowWithNoRecordingOnDiskCarriesNoAudio(t *testing.T) {
	groups := groupByDay([]history.Entry{{
		Timestamp: time.Date(2026, 8, 18, 14, 22, 1, 0, time.Local),
		Text:      "a dictation",
		AudioPath: "/nowhere/gone.wav",
	}}, nil)
	if got := groups[0].Entries[0].Audio; got != "" {
		t.Errorf("got %q, want empty for a path that does not exist", got)
	}
}

// ShowMainWindow reports a loopback bind failure through this seam (see
// startPageServer's error branch) since internal/ui cannot call main's
// notify() directly. Driving an actual bind failure would need a mocked
// net.Listen -- not practical to force deterministically in a unit test --
// so this covers the seam itself: SetNotifier installs a callback that is
// reachable and gets called with what it's given.
func TestSetNotifierInstallsAReachableCallback(t *testing.T) {
	var got string
	SetNotifier(func(msg string) { got = msg })
	t.Cleanup(func() { SetNotifier(nil) })

	winMu.Lock()
	fn := notifyUser
	winMu.Unlock()
	if fn == nil {
		t.Fatal("SetNotifier did not install a callback")
	}
	fn("Could not open the window.")
	if got != "Could not open the window." {
		t.Errorf("got %q, want the message passed through unchanged", got)
	}
}

func TestMainPageCarriesEveryPaneAndEveryMarker(t *testing.T) {
	page, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	for _, pane := range []string{"overview", "history", "meetings", "settings"} {
		if !strings.Contains(body, `data-pane="`+pane+`"`) {
			t.Errorf("no %s pane", pane)
		}
	}
	for _, marker := range []string{kitCSSMarker, kitJSMarker, settingsCSSMarker, indicatorCSSMarker, settingsMarkup} {
		if !strings.Contains(body, marker) {
			t.Errorf("marker %q is missing", marker)
		}
	}
}

// Every marker has to resolve, or the window opens unstyled or half-built.
func TestBuildMainPageResolvesEveryMarker(t *testing.T) {
	page := buildMainPage()
	for _, marker := range []string{kitCSSMarker, kitJSMarker, settingsCSSMarker, indicatorCSSMarker, settingsMarkup} {
		if strings.Contains(page, marker) {
			t.Errorf("marker %q was left unspliced", marker)
		}
	}
	if !strings.Contains(page, `id="settings-pane"`) {
		t.Error("the settings fragment did not make it into the page")
	}
}

// The Copy button must stay a copy button whatever confirming a row is set
// to do, and a settings file from the popover era must not leave the paste
// option unreachable.
func TestConfirmModeResolvesTheHistorySetting(t *testing.T) {
	cases := []struct {
		setting  string
		copyOnly bool
		want     string
	}{
		{output.ModeNone, false, output.ModeNone},
		{output.ModeNone, true, output.ModeNone},
		{output.ModeCopy, false, output.ModeCopy},
		{output.ModePaste, false, output.ModePaste},
		{output.ModePasteCopy, false, output.ModePaste},
		{output.ModePaste, true, output.ModeCopy},
		{"", false, output.ModeCopy},
	}
	for _, c := range cases {
		if got := confirmMode(c.setting, c.copyOnly); got != c.want {
			t.Errorf("confirmMode(%q, %v) = %q, want %q", c.setting, c.copyOnly, got, c.want)
		}
	}
}

// A notification's action string decides which pane the window opens on, and
// it can come from a banner posted by an older build. selectPane("") does not
// mean "leave it alone" -- it turns every pane off and leaves a blank window.
func TestUnknownPanesLandOnOverview(t *testing.T) {
	for _, pane := range []string{PaneOverview, PaneHistory, PaneMeetings, PaneTasks, PaneSettings} {
		if got := normalizePane(pane); got != pane {
			t.Errorf("normalizePane(%q) = %q, want it unchanged", pane, got)
		}
	}
	for _, junk := range []string{"", "voices", "Settings", "overview "} {
		if got := normalizePane(junk); got != PaneOverview {
			t.Errorf("normalizePane(%q) = %q, want %q", junk, got, PaneOverview)
		}
	}
}

// The five names the page's own nav keys off. Spelled out here because the
// notification actions, the tray menu and internal/task all write them by
// hand somewhere, and a rename that misses one is silent.
func TestPaneNamesMatchThePage(t *testing.T) {
	want := map[string]string{
		PaneOverview: "overview",
		PaneHistory:  "history",
		PaneMeetings: "meetings",
		PaneTasks:    "tasks",
		PaneSettings: "settings",
	}
	page, err := assets.ReadFile("assets/main.html")
	if err != nil {
		t.Fatal(err)
	}
	for constant, literal := range want {
		if constant != literal {
			t.Errorf("constant is %q, want %q", constant, literal)
		}
		if !strings.Contains(string(page), `data-pane="`+literal+`"`) {
			t.Errorf("the page has no pane named %q", literal)
		}
	}
}
