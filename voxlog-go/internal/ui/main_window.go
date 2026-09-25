package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	webview "github.com/webview/webview_go"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/history"
	"voxlog-go/internal/hotkey"
	"voxlog-go/internal/llm"
	"voxlog-go/internal/output"
	"voxlog-go/internal/permissions"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/task"
	"voxlog-go/internal/usernotify"
)

// mainWin is the app's one window, if it is currently open.
// ponytail: package-level var is the whole "window manager" needed here --
// the app has exactly one window and four panes inside it. winMu guards the
// check-then-act between the nil check and the goroutine that actually
// assigns mainWin, closing the race where two quick calls both see nil and
// both spawn a window.
var (
	winMu       sync.Mutex
	mainWin     webview.WebView
	mainOpening bool
	// pageSrv serves the window's HTML and its recordings over loopback.
	// Started once per process (the first ShowMainWindow call), not once per
	// window: the token and the listener are cheap to keep around and
	// pointless to churn on every reopen.
	pageSrv *pageServer
)

const (
	mainWidth  = 980
	mainHeight = 660
)

// OpenSettingsFlag is the argument a relaunched instance is started with
// to reopen the Settings window (see the restartApp binding).
const OpenSettingsFlag = "--open-settings"

// transcribeHandler decodes a stored meeting recording. Set once at startup
// by the app (see SetTranscribeHandler) -- this package cannot reach the
// models or the decode queue itself, and a callback is a smaller seam than
// threading the whole app through every window constructor.
var transcribeHandler func(time.Time) error

// SetTranscribeHandler installs what the Transcribe button on a meeting entry
// does.
func SetTranscribeHandler(fn func(time.Time) error) {
	winMu.Lock()
	transcribeHandler = fn
	winMu.Unlock()
}

// refingerprintHandler rebuilds the voice fingerprints of a meeting after a
// person has corrected who spoke. Set once at startup by the app, for the same
// reason SetTranscribeHandler is: describing a voice means running the
// embedding model, and this package cannot reach it.
//
// It is what makes a correction worth more than an edit -- without it the
// transcript reads right and the next meeting makes the same mistake.
var refingerprintHandler func(time.Time)

// SetRefingerprintHandler installs the callback above.
func SetRefingerprintHandler(fn func(time.Time)) {
	winMu.Lock()
	refingerprintHandler = fn
	winMu.Unlock()
}

// refingerprint runs that callback if the app installed one.
func refingerprint(at time.Time) {
	winMu.Lock()
	fn := refingerprintHandler
	winMu.Unlock()
	if fn != nil {
		fn(at)
	}
}

// notifyUser shows the user a message. Set once at startup by the app (see
// SetNotifier) -- this package cannot call main's notify() directly (it is a
// different package, and main cannot import ui without a cycle), and a
// callback is the smallest seam that lets a bind failure here reach the same
// notification banner every other user-visible failure in the app already
// uses.
var notifyUser func(string)

// SetNotifier installs how this package tells the user something went wrong
// when there is no window to show it in -- today, only a loopback bind
// failure (see ShowMainWindow).
func SetNotifier(fn func(string)) {
	winMu.Lock()
	notifyUser = fn
	winMu.Unlock()
}

// systemAudioErrFn reports why the system-audio tap is not delivering, or ""
// when it is fine. Installed by the app: the tap lives in package main's
// always-on supervisor, and a failure there is invisible to the user unless
// Settings can ask about it.
var systemAudioErrFn func() string

// SetSystemAudioErrorFunc installs the callback above.
func SetSystemAudioErrorFunc(fn func() string) {
	winMu.Lock()
	systemAudioErrFn = fn
	winMu.Unlock()
}

func systemAudioError() string {
	winMu.Lock()
	fn := systemAudioErrFn
	winMu.Unlock()
	if fn == nil {
		return ""
	}
	return fn()
}

// sweepHandler is what the freeUpSpace binding runs. Set once at startup by
// the app (see SetSweepHandler) -- this package has no way to know which
// recording, if any, a live meeting is still writing to, so it cannot run
// the sweep itself without risking a file out from under an open handle.
var sweepHandler func()

// SetSweepHandler installs the app's cleanup pass (settings plus in-use
// guard) behind the Free up space button.
func SetSweepHandler(fn func()) {
	winMu.Lock()
	sweepHandler = fn
	winMu.Unlock()
}

// meetingStatusHandler is what the Overview banner's poll asks to find out
// whether a meeting is running, for how long, and whether the microphone is
// muted. Set once at startup, for the same reason SetTranscribeHandler is:
// this package cannot reach the app's own state (package main, guarded by its
// own lock).
//
// Muted rides along with the poll rather than having a binding of its own
// because the menu bar can mute too: the banner has to follow a mute it did
// not make, and the poll is the only thing that ever asks.
var meetingStatusHandler func() (bool, float64, bool)

// SetMeetingStatusHandler installs the callback above.
func SetMeetingStatusHandler(fn func() (bool, float64, bool)) {
	winMu.Lock()
	meetingStatusHandler = fn
	winMu.Unlock()
}

// toggleMuteHandler silences the user's own microphone for the rest of the
// recording, and reports what the microphone is after the flip. The same
// method the menu bar item calls.
var toggleMuteHandler func() bool

// SetToggleMuteHandler installs the callback above.
func SetToggleMuteHandler(fn func() bool) {
	winMu.Lock()
	toggleMuteHandler = fn
	winMu.Unlock()
}

// stopMeetingHandler ends a running meeting. Set once at startup -- the same
// method the tray's "Stop meeting recording" item and the meeting hotkey
// already call, so the Overview banner's Stop button goes through the one
// place that logic lives rather than a second copy of it.
var stopMeetingHandler func()

// SetStopMeetingHandler installs the callback above.
func SetStopMeetingHandler(fn func()) {
	winMu.Lock()
	stopMeetingHandler = fn
	winMu.Unlock()
}

type historyEntryJSON struct {
	Time             string  `json:"time"`
	DurationSeconds  float64 `json:"duration_seconds"`
	Text             string  `json:"text"`
	RecordingSeconds float64 `json:"recording_seconds"`
	// Audio is the recording's base name, or "" if it is not on disk. The
	// page fetches it from the loopback server by name, so no absolute path
	// (and no fragment of the user's home directory) needs to reach it.
	Audio string `json:"audio"`
	// HasAudio decides if the Transcribe button shows; it is just Audio's
	// presence, not a second disk check.
	HasAudio bool `json:"has_audio"`
	// ID is the entry's timestamp, the handle the Transcribe action passes
	// back to find it again.
	ID string `json:"id"`
	// TaskID is set when Task Hub classified this entry as an actionable
	// task -- its presence is what draws the row's green accent and "Task
	// detected" tag. TaskEntity/TaskStatus/TaskReminder ride along so the row
	// can show the project/status/reminder without a second round trip.
	TaskID       string `json:"task_id,omitempty"`
	TaskEntity   string `json:"task_entity,omitempty"`
	TaskStatus   string `json:"task_status,omitempty"`
	TaskReminder string `json:"task_reminder,omitempty"`
	// AutoStarted marks a note always-on recorded on its own. Shown, because
	// a line nobody dictated must not read like one that was.
	AutoStarted bool `json:"auto_started,omitempty"`
}

// taskBySource maps a task's SourceKey to itself, so a history/meeting row
// can be joined against Task Hub's list without an O(n*m) scan per row.
func taskBySource(tasks []task.Task) map[string]task.Task {
	m := make(map[string]task.Task, len(tasks))
	for _, t := range tasks {
		m[t.SourceKey] = t
	}
	return m
}

// recordingName returns path's base name if the file still stats, or "" if
// it doesn't -- the one rule HasAudio and the Audio field both rely on, so a
// stale AudioPath left over after a sweep never offers a player or a
// Transcribe button for a file that is no longer there.
func recordingName(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return filepath.Base(path)
}

type dayGroupJSON struct {
	Day     string             `json:"day"`
	Entries []historyEntryJSON `json:"entries"`
}

func groupByDay(entries []history.Entry, tasks map[string]task.Task) []dayGroupJSON {
	byDay := map[string][]historyEntryJSON{}
	for _, e := range entries {
		// Meetings that haven't migrated out of the day files yet (failed
		// migration, restored backup, sentinel copied ahead of the data)
		// belong to the Meetings pane, not the Dictations one.
		if e.Kind == history.KindMeeting {
			continue
		}
		day := e.Timestamp.Format("2006-01-02")
		audio := recordingName(e.AudioPath)
		id := e.Timestamp.Format(time.RFC3339Nano)
		row := historyEntryJSON{
			Time:             e.Timestamp.Format("15:04"),
			DurationSeconds:  e.DurationSeconds,
			Text:             e.Text,
			RecordingSeconds: e.RecordingSeconds,
			Audio:            audio,
			HasAudio:         audio != "",
			ID:               id,
			AutoStarted:      e.AutoStarted,
		}
		if t, ok := tasks[id]; ok {
			row.TaskID = t.ID
			row.TaskEntity = t.Entity
			row.TaskStatus = string(t.Status)
			if t.Reminder != nil {
				row.TaskReminder = t.Reminder.Format("Jan 2 15:04")
			}
		}
		byDay[day] = append(byDay[day], row)
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))

	groups := make([]dayGroupJSON, 0, len(days))
	for _, d := range days {
		// Newest first inside each day too, not just across days: entries
		// arrive in append (oldest-first) order, and the most recent
		// dictation is the one worth seeing without scrolling.
		entries := byDay[d]
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
		groups = append(groups, dayGroupJSON{Day: d, Entries: entries})
	}
	return groups
}

type meetingJSON struct {
	ID               string  `json:"id"`
	Start            string  `json:"time"`
	Day              string  `json:"day"`
	RecordingSeconds float64 `json:"recording_seconds"`
	// DurationSeconds is how long the transcript took to decode, 0 while the
	// meeting has none. Both numbers travel: the row shows the decode time in
	// its pill, the length of the call in the line underneath.
	DurationSeconds float64 `json:"duration_seconds"`
	Text            string  `json:"text"`
	// Summary is Task Hub's short LLM summary, "" until it lands (or Task
	// Hub is off).
	Summary string `json:"summary,omitempty"`
	// Title is the name summarization gave the meeting, "" for one recorded
	// before titles existed or summarized with none. The row falls back to
	// Day/Start, which is what every meeting used to show.
	Title string `json:"title,omitempty"`
	// Audio is the recording's base name, or "" if it is not on disk -- see
	// historyEntryJSON.Audio.
	Audio string `json:"audio"`
	// SystemAudio is the other participants' side of the call -- a meeting
	// keeps the two tracks as separate WAV files (see meeting.go) rather than
	// mixing them, so the row needs a second name to offer a second player.
	// "" if that file is not on disk, by the same rule as Audio.
	SystemAudio string `json:"system_audio"`
	// HasAudio requires the file to still stat, not just AudioPath to be
	// non-empty: from phase 3 onward the recordings directory is the source
	// of truth, and a stale path left in the record must not offer a
	// Transcribe button that would then fail. Derived from Audio rather than
	// statting a second time.
	HasAudio bool `json:"has_audio"`
	// TaskID/TaskEntity/TaskStatus/TaskReminder mirror historyEntryJSON's -- see there.
	TaskID       string `json:"task_id,omitempty"`
	TaskEntity   string `json:"task_entity,omitempty"`
	TaskStatus   string `json:"task_status,omitempty"`
	TaskReminder string `json:"task_reminder,omitempty"`
	// Entity is the project the meeting was filed under by hand, which is
	// what makes the project filter work for a meeting Task Hub found nothing
	// in. TaskEntity above is still the fallback.
	Entity string `json:"entity,omitempty"`
	// Speakers is who spoke and for how long -- enough to draw the row's
	// talk-time bar and its avatars. The replies themselves are NOT here: an
	// hour-long call is thousands of them and this payload is rebuilt on
	// every refresh, so they are fetched when a meeting is opened.
	Speakers []speakerJSON `json:"speakers,omitempty"`
	// HasTurns says whether opening this meeting will show replies with
	// playable timings, or only the flat transcript of an older decode.
	HasTurns bool `json:"has_turns"`
}

// meetingsJSON is the flat sibling of groupByDay: meetings are few enough per
// day that grouping them lives entirely in JS (see renderMeetings), and a
// flat list is what MeetingStore.All already hands back.
// withSpeakers fills in who spoke in each meeting. Separate from meetingsJSON
// so the shape of a row stays testable without a database behind it.
func withSpeakers(rows []meetingJSON, ms []history.Meeting, speakers map[int64][]history.MeetingSpeaker) []meetingJSON {
	for i := range rows {
		if i < len(ms) {
			rows[i].Speakers = speakersJSON(speakers[ms[i].Start.UnixNano()])
		}
	}
	return rows
}

func meetingsJSON(ms []history.Meeting, tasks map[string]task.Task) []meetingJSON {
	out := make([]meetingJSON, 0, len(ms))
	for _, m := range ms {
		audio := recordingName(m.AudioPath)
		id := m.Start.Format(time.RFC3339Nano)
		row := meetingJSON{
			ID:               id,
			Start:            m.Start.Format("15:04"),
			Day:              m.Start.Format("2006-01-02"),
			RecordingSeconds: m.RecordingSeconds,
			DurationSeconds:  m.DurationSeconds,
			Text:             m.Text,
			Summary:          m.Summary,
			Title:            m.Title,
			Audio:            audio,
			SystemAudio:      recordingName(m.SystemAudioPath),
			HasAudio:         audio != "",
			Entity:           m.Entity,
			HasTurns:         m.TurnsVersion > 0,
		}
		if t, ok := tasks[id]; ok {
			row.TaskID = t.ID
			row.TaskEntity = t.Entity
			row.TaskStatus = string(t.Status)
			if t.Reminder != nil {
				row.TaskReminder = t.Reminder.Format("Jan 2 15:04")
			}
		}
		out = append(out, row)
	}
	return out
}

type taskJSON struct {
	ID         string `json:"id"`
	SourceKind string `json:"source_kind"`
	SourceKey  string `json:"source_key"`
	Text       string `json:"text"`
	Entity     string `json:"entity"`
	Status     string `json:"status"`
	// Reminder is RFC3339 for the frontend to parse and format/compare
	// against "now" itself (for the overdue styling); "" means none set.
	Reminder string `json:"reminder,omitempty"`
	Created  string `json:"created"`
	// Notes is the user's free text (task detail view); Updated is RFC3339
	// or "" for a task nobody has edited by hand.
	Notes   string `json:"notes,omitempty"`
	Updated string `json:"updated,omitempty"`
}

func tasksJSON(tasks []task.Task) []taskJSON {
	out := make([]taskJSON, 0, len(tasks))
	for _, t := range tasks {
		row := taskJSON{
			ID:         t.ID,
			SourceKind: t.SourceKind,
			SourceKey:  t.SourceKey,
			Text:       t.Text,
			Entity:     t.Entity,
			Status:     string(t.Status),
			Created:    t.Created.Format(time.RFC3339),
			Notes:      t.Notes,
		}
		if !t.Updated.IsZero() {
			row.Updated = t.Updated.Format(time.RFC3339)
		}
		if t.Reminder != nil {
			row.Reminder = t.Reminder.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// macOS answers AXIsProcessTrusted and CGPreflightScreenCaptureAccess once
// per process and caches the result until the app restarts -- which is why
// the pane tells the user to relaunch after granting either. Asking them
// again on every poll therefore cannot produce a new answer; all it produces
// is a TCC round trip on the main thread, several times a minute, for as
// long as the window exists. Ask once, remember it.
var (
	staticPermsOnce   sync.Once
	staticAccess      string
	staticScreenShare string
)

func cachedStaticPermissions() (accessibility, screenRecording string) {
	staticPermsOnce.Do(func() {
		staticAccess = accessibilityStatus()
		staticScreenShare = screenRecordingStatus()
	})
	return staticAccess, staticScreenShare
}

func screenRecordingStatus() string {
	if permissions.ScreenRecording() {
		return "granted"
	}
	return "denied"
}

func accessibilityStatus() string {
	if permissions.Accessibility() {
		return "granted"
	}
	return "denied"
}

type modelInfoJSON struct {
	Family     string `json:"family"`
	Variant    string `json:"variant"`
	Downloaded bool   `json:"downloaded"`
	// Capability flags drive which controls the form shows at all: a
	// language picker or a "live streaming text" checkbox that the selected
	// engine would silently ignore is worse than no control.
	SupportsLanguage  bool   `json:"supports_language"`
	SupportsStreaming bool   `json:"supports_streaming"`
	Description       string `json:"description"`
}

// micTest is the Settings pane's live input meter: a capture stream whose
// level is pushed to the page while the user drags the gain slider, so they
// can set it by ear/eye instead of guessing and re-recording.
type micTest struct {
	mu       sync.Mutex
	recorder *audio.Recorder
}

// modelComparison records one take and runs it through every downloaded
// model, so the choice between them can be made on the user's own voice
// rather than on a description. It holds the audio only between start and
// finish, and never writes it anywhere.
type modelComparison struct {
	mu       sync.Mutex
	recorder *audio.Recorder
}

func (c *modelComparison) start(deviceName string, gain float64) error {
	c.cancel()

	rec, err := audio.NewRecorder(deviceName, gain)
	if err != nil {
		return err
	}
	if err := rec.Start(); err != nil {
		rec.Close()
		return err
	}
	c.mu.Lock()
	c.recorder = rec
	c.mu.Unlock()
	return nil
}

func (c *modelComparison) cancel() {
	c.mu.Lock()
	rec := c.recorder
	c.recorder = nil
	c.mu.Unlock()
	if rec != nil {
		rec.Stop()
		rec.Close()
	}
}

// finish stops the recording and transcribes it once per downloaded model,
// timing each. Models are loaded one at a time and released immediately:
// holding several of these open at once means several gigabytes resident for
// no reason, since they are used strictly in sequence here.
func (c *modelComparison) finish(specs []asr.ModelSpec, modelsBaseDir, language string) ([]map[string]any, error) {
	c.mu.Lock()
	rec := c.recorder
	c.recorder = nil
	c.mu.Unlock()
	if rec == nil {
		return nil, fmt.Errorf("no recording in progress")
	}
	samples := rec.Stop()
	rec.Close()

	if len(samples) == 0 {
		return nil, fmt.Errorf("nothing was recorded")
	}

	results := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		if !asr.IsDownloaded(modelsBaseDir, spec) {
			continue
		}
		row := map[string]any{"family": spec.Family, "variant": spec.Variant}

		started := time.Now()
		transcriber, err := asr.NewTranscriber(spec, asr.ModelDir(modelsBaseDir, spec), language)
		if err != nil {
			row["error"] = err.Error()
			results = append(results, row)
			continue
		}
		loaded := time.Now()
		text, err := transcriber.Transcribe(samples, language)
		transcriber.Close()
		if err != nil {
			row["error"] = err.Error()
			results = append(results, row)
			continue
		}
		row["text"] = text
		// Load time is reported apart from decode time: the first run of a
		// model pays for reading gigabytes off disk, which says nothing about
		// how fast it transcribes.
		row["load_seconds"] = loaded.Sub(started).Seconds()
		row["seconds"] = time.Since(loaded).Seconds()
		results = append(results, row)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no downloaded models to compare")
	}
	return results, nil
}

// start opens deviceName at gain and streams its level to onLevel until
// stop is called. Restarts cleanly if a test is already running (e.g. the
// user switched device or nudged the gain slider mid-test).
func (m *micTest) start(deviceName string, gain float64, onLevel func(float64)) error {
	m.stop()

	rec, err := audio.NewRecorder(deviceName, gain)
	if err != nil {
		return err
	}

	var lastPush time.Time
	if err := rec.StartStreaming(func(chunk []float32) {
		// Runs on miniaudio's realtime thread; throttle the webview hop so
		// a 16kHz stream can't flood the main queue (same reasoning as the
		// recording overlay's level feed).
		if time.Since(lastPush) < 40*time.Millisecond {
			return
		}
		lastPush = time.Now()
		// Measured before gain (see Recorder.Level): a meter that moves with
		// the slider instead of with the voice cannot be used to set the
		// slider.
		onLevel(rec.Level())
	}); err != nil {
		rec.Close()
		return err
	}

	m.mu.Lock()
	m.recorder = rec
	m.mu.Unlock()
	return nil
}

func (m *micTest) stop() {
	m.mu.Lock()
	rec := m.recorder
	m.recorder = nil
	m.mu.Unlock()

	if rec != nil {
		rec.Stop()
		rec.Close()
	}
}

type downloadProgressJSON struct {
	FileFamily  string `json:"file_family"`
	FileVariant string `json:"file_variant"`
	Downloaded  int64  `json:"downloaded"`
	Total       int64  `json:"total"`
}

type downloadDoneJSON struct {
	Family  string `json:"family"`
	Variant string `json:"variant"`
	Error   string `json:"error,omitempty"`
}

// httpFetch is the production asr.FetchFunc: a plain net/http GET.
// ponytail: no retry/resume logic — asr.Download already skips files that
// exist and are non-empty, so a re-run after a failure just restarts the
// partial file from zero. Upgrade to range requests if large-model users
// complain about restarting downloads.
func httpFetch(url string) (io.ReadCloser, int64, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

// The panes the window has, as the strings the page's own nav keys off (see
// assets/main.html's #shell-nav). Named here rather than spelled out at every
// caller because they are also the action a notification carries: a click on
// a banner arrives as one of these, and a string nothing recognises used to
// reach the page as-is.
const (
	PaneOverview = "overview"
	PaneHistory  = "history"
	PaneMeetings = "meetings"
	PaneTasks    = "tasks"
	PaneSettings = "settings"
	// PaneWelcome is not a pane in the sidebar: it is the first-run welcome,
	// drawn over Overview (see assets/welcome.html). It rides the same pane
	// argument so the app can open the window straight onto it.
	PaneWelcome = "welcome"
)

// normalizePane maps anything unrecognised -- including "" -- onto Overview.
//
// selectPane("") does not mean "leave the pane alone": it turns every pane
// off and leaves a blank window (see refreshMainWindow). The window is opened
// by notification clicks, whose action string can come from a banner posted
// by an older build, so an unknown pane has to land somewhere real.
func normalizePane(pane string) string {
	switch pane {
	case PaneOverview, PaneHistory, PaneMeetings, PaneTasks, PaneSettings, PaneWelcome:
		return pane
	}
	return PaneOverview
}

// ShowMainWindow opens the window on the named pane (one of the Pane
// constants above), or brings it forward on that pane if it is already open.
// recordingsDir is where dictation and meeting audio are kept; it is the only
// directory the loopback server (started here, once) is allowed to serve
// files from.
func ShowMainWindow(pane string, store *history.Store, meetings *history.MeetingStore, tasks *task.Store, cfgStore *settings.Store, models []asr.ModelSpec, modelsBaseDir, recordingsDir string) {
	pane = normalizePane(pane)
	winMu.Lock()
	if pageSrv == nil {
		srv, err := startPageServer(recordingsDir, func(id int64) ([]byte, error) {
			return voiceClipHandler(meetings, id)
		})
		if err != nil {
			fn := notifyUser
			winMu.Unlock()
			// No page, no window: a blank WKWebView with nothing behind it
			// would be a worse failure mode than not opening at all. This is
			// fatal to the window only, not the app -- the menu bar and its
			// hotkeys keep working, so the user needs a reason the window
			// itself didn't show up.
			log.Printf("start page server: %v", err)
			if fn != nil {
				fn("Could not open the window.")
			}
			return
		}
		pageSrv = srv
	}
	if mainWin != nil {
		w := mainWin
		if windowUsable(w) {
			winMu.Unlock()
			refreshMainWindow(w, pane, store, meetings, tasks, cfgStore, models, modelsBaseDir)
			return
		}
		// The red X ran webview's own windowWillClose:, which nils its
		// internal NSWindow/WKWebView pointers -- the handle survives but
		// every call through it is inert (an 0x0, invisible "window").
		// Nothing here can revive that, so drop it and build a fresh one.
		// The dead C++ object is deliberately leaked rather than Destroy()d:
		// it has already torn itself down, and a second teardown is a
		// double-free.
		mainWin = nil
	}
	if mainOpening {
		winMu.Unlock()
		return
	}
	mainOpening = true
	srv := pageSrv
	winMu.Unlock()
	// Window construction and its whole Run() event loop must happen on the
	// real OS main thread (see mainthread.go) -- a bare `go` here used to
	// crash AppKit's "NSWindow should only be instantiated on the main
	// thread!" assertion.
	runOnMain(func() {
		runMainWindow(pane, store, meetings, tasks, cfgStore, models, modelsBaseDir, recordingsDir, srv)
	})
}

// windowUsable reports whether a webview handle still refers to a live,
// showable window. See ShowMainWindow for why this is needed.
func windowUsable(w webview.WebView) bool {
	win := w.Window()
	if win == nil {
		return false
	}
	_, _, width, height, _ := describeWindow(win)
	return width > 0 && height > 0
}

// MainWindowVisible reports whether the window is currently on screen (open
// and not hidden). Called from hotkey handlers, so the frame is measured on
// the main thread and the answer handed back (see runOnMainSync) -- reading an
// NSWindow from a goroutine is the AppKit rule this package keeps everywhere
// else.
func MainWindowVisible() bool {
	winMu.Lock()
	w := mainWin
	winMu.Unlock()
	if w == nil {
		return false
	}
	var visible bool
	runOnMainSync(func() {
		if !windowUsable(w) {
			return
		}
		_, _, _, _, visible = describeWindow(w.Window())
	})
	return visible
}

// HideMainWindow orders the window off screen without closing it, so it can
// be shown again instantly. Used by Escape and by pressing the History
// hotkey a second time.
func HideMainWindow() {
	winMu.Lock()
	w := mainWin
	winMu.Unlock()
	if w == nil || !windowUsable(w) {
		return
	}
	runOnMain(func() { hideWindow(w.Window()) })
}

// confirmMode resolves the History setting and which control was used into
// one of ModeNone/ModeCopy/ModePaste. "paste_copy" is a popover-era value
// the picker no longer offers: it asked for a paste, and paste already
// leaves the clipboard as it found it.
func confirmMode(setting string, copyOnly bool) string {
	if setting == output.ModeNone {
		return output.ModeNone
	}
	if copyOnly {
		return output.ModeCopy
	}
	if setting == output.ModePaste || setting == output.ModePasteCopy {
		return output.ModePaste
	}
	return output.ModeCopy
}

// prevAppPid is the app that was frontmost before the window took focus, so
// a confirmed entry can be pasted back where the user actually was. Guarded
// by winMu, like everything else about the window's state.
var prevAppPid int

// rememberFrontmostApp records where the user is right now.
//
// MUST be called on the real OS main thread, immediately before the window
// takes focus -- it is an AppKit call (NSWorkspace), and this package's rule
// is that every one of those runs inside a runOnMain/Dispatch closure. Called
// from a goroutine it is a thread-safety violation of exactly the kind that
// freezes the whole process: AppKit takes its own locks, and the main thread
// blocks behind them the next time it draws anything.
//
// Our own pid is never stored: after the window is up the frontmost app is
// Voxlog, and a second History press must not overwrite the app the first one
// remembered.
func rememberFrontmostApp() {
	pid := FrontmostAppPid()
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	winMu.Lock()
	prevAppPid = pid
	winMu.Unlock()
}

// focusReturnDelay is how long the app being activated gets to actually
// become frontmost before Cmd+V is sent. Activation is asynchronous: paste
// too early and the keystroke lands in the window that is on its way out.
const focusReturnDelay = 220 * time.Millisecond

// pasteEntryBack puts the window away, returns focus to the app it was
// taken from, and pastes. Runs off the binding's goroutine: it sleeps, and
// the hide it is waiting on cannot happen while the main thread is blocked.
func pasteEntryBack(text string) {
	HideMainWindow()
	winMu.Lock()
	pid := prevAppPid
	winMu.Unlock()
	if pid > 0 {
		runOnMain(func() { ActivateAppByPid(pid) })
	}
	time.Sleep(focusReturnDelay)
	if err := output.Emit(text, output.ModePaste); err != nil {
		log.Printf("paste history entry: %v", err)
	}
}

// RefreshMainWindowIfOpen updates the lists in place, without bringing the
// window forward. Used when a meeting's transcript arrives minutes after the
// call ended: the list must stop saying "not transcribed", but stealing focus
// from whatever the user moved on to would be worse than a stale row.
func RefreshMainWindowIfOpen(store *history.Store, meetings *history.MeetingStore, tasks *task.Store) {
	// The drawer shows the same tasks from the same store, so anything that
	// refreshes the window refreshes it too -- otherwise a task classified
	// while the drawer is open sits missing from it until it is reopened.
	refreshDrawer()

	winMu.Lock()
	w := mainWin
	winMu.Unlock()
	if w == nil || !windowUsable(w) {
		return
	}
	entries, err := store.AllEntries()
	if err != nil {
		return
	}
	taskList, err := tasks.All()
	if err != nil {
		taskList = nil
	}
	bySource := taskBySource(taskList)
	daysData, _ := json.Marshal(groupByDay(entries, bySource))
	meetingList, err := meetings.All()
	if err != nil {
		meetingList = nil
	}
	meetingSpeakers, err := meetings.SpeakersByMeeting()
	if err != nil {
		log.Printf("reading meeting speakers: %v", err)
	}
	meetingsData, _ := json.Marshal(withSpeakers(meetingsJSON(meetingList, bySource), meetingList, meetingSpeakers))
	tasksData, _ := json.Marshal(tasksJSON(taskList))
	// The rejected list travels with the tasks: Settings' "Not a task"
	// section is redrawn by the same refresh that redraws the Tasks pane,
	// which is what makes Remove/"Actually a task" take effect immediately.
	rejected, err := tasks.LoadRejected()
	if err != nil {
		log.Printf("reading rejected tasks: %v", err)
	}
	if rejected == nil {
		rejected = []string{}
	}
	rejectedData, _ := json.Marshal(rejected)
	// One more call over lists already in hand, not one more read of the
	// disk -- entries and meetingList are already loaded above.
	overviewData, _ := json.Marshal(buildOverview(entries, meetingList, time.Now()))
	runOnMain(func() {
		w.Eval(fmt.Sprintf(
			"window.voxlog = window.voxlog || {}; window.voxlog.days = %s; window.voxlog.meetings = %s; window.voxlog.overview = %s; window.voxlog.tasks = %s; window.voxlog.rejected = %s; window.voxlog.decodeQueue = %s; typeof render === 'function' && render(); typeof renderRejected === 'function' && renderRejected();",
			daysData, meetingsData, overviewData, tasksData, rejectedData, decodeQueueJSON(),
		))
	})
}

// refreshMainWindow re-shows the window on the requested pane with current
// data. Both halves of the page are refilled whichever pane was asked for:
// they share one window now, and the one the user did not ask for is a click
// away rather than a reload away.
func refreshMainWindow(w webview.WebView, pane string, store *history.Store, meetings *history.MeetingStore, tasks *task.Store, cfgStore *settings.Store, models []asr.ModelSpec, modelsBaseDir string) {
	pane = normalizePane(pane)
	daysData, meetingsData, overviewData, tasksData, rejectedData := pageData(store, meetings, tasks)
	settingsData, modelsData, llmData := settingsJSON(cfgStore, models, modelsBaseDir)
	runOnMain(func() {
		// Re-show, not just re-populate: this same window survives its close
		// button (keepAliveOnClose), so reopening it lands here with an
		// ordered-out window that still needs putting back on screen.
		rememberFrontmostApp()
		activateApp()
		showWindow(w.Window())
		w.Eval(fmt.Sprintf(
			"window.voxlog = window.voxlog || {}; window.voxlog.days = %s; window.voxlog.meetings = %s; window.voxlog.overview = %s; window.voxlog.tasks = %s; window.voxlog.rejected = %s; window.voxlog.decodeQueue = %s; window.voxlog.settings = %s; window.voxlog.models = %s; window.voxlog.llmModel = %s; typeof render === 'function' && render(); typeof fillForm === 'function' && fillForm(); typeof refreshPermissions === 'function' && refreshPermissions();",
			daysData, meetingsData, overviewData, tasksData, rejectedData, decodeQueueJSON(), settingsData, modelsData, llmData,
		))
		w.Eval(fmt.Sprintf("window.selectPane && window.selectPane(%q);", pane))
		// Reopening lands here rather than in the page's own startup, so the
		// keyboard selection has to be re-armed from this side too -- otherwise
		// the first arrow press after a reopen is spent creating a selection
		// instead of moving one.
		w.Eval("window.kbdSelectFirstRow && window.kbdSelectFirstRow();")
	})
}

// keepAppOwned carries over the fields the page never sends. The Settings
// form builds its payload field by field, so anything it does not list
// arrives zeroed -- and a zeroed settings_version re-runs every migration on
// the next launch, while a zeroed welcome_done would open the welcome again.
func keepAppOwned(v, stored settings.Settings) settings.Settings {
	v.Version = stored.Version
	v.WelcomeDone = stored.WelcomeDone
	v.WelcomeStep = stored.WelcomeStep
	return v
}

// humanBytes renders a byte count the way the settings pane shows it
// ("4.6 GB"). Decimal (1000-based), not binary, to match AudioMaxGB's own
// gigabyte-to-byte conversion (see app.sweepRecordings) -- the number on
// screen and the ceiling the user typed should agree on what a GB is.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// pageData reads entries and meetings once and marshals every list the page
// needs from that single read, overview included -- so opening or
// refreshing the window costs one disk read, not one per list it fills.
func pageData(store *history.Store, meetings *history.MeetingStore, tasks *task.Store) (daysData, meetingsData, overviewData, tasksData, rejectedData []byte) {
	entries, err := store.AllEntries()
	if err != nil {
		entries = nil
	}
	meetingList, err := meetings.All()
	if err != nil {
		meetingList = nil
	}
	taskList, err := tasks.All()
	if err != nil {
		taskList = nil
	}
	bySource := taskBySource(taskList)
	daysData, _ = json.Marshal(groupByDay(entries, bySource))
	meetingSpeakers, err := meetings.SpeakersByMeeting()
	if err != nil {
		log.Printf("reading meeting speakers: %v", err)
	}
	meetingsData, _ = json.Marshal(withSpeakers(meetingsJSON(meetingList, bySource), meetingList, meetingSpeakers))
	overviewData, _ = json.Marshal(buildOverview(entries, meetingList, time.Now()))
	tasksData, _ = json.Marshal(tasksJSON(taskList))
	// The rejected list rides along with the tasks it is the mirror image of,
	// so Settings' "Not a task" section refreshes with everything else
	// instead of needing a fetch of its own.
	rejected, err := tasks.LoadRejected()
	if err != nil {
		log.Printf("reading rejected tasks: %v", err)
	}
	if rejected == nil {
		rejected = []string{}
	}
	rejectedData, _ = json.Marshal(rejected)
	return
}

// llmModelJSON tells Settings' LLM subpane whether the classification model
// is already on disk -- same "Downloaded" idea as modelInfoJSON, kept
// separate since it isn't part of the ASR model list the Model subpane owns.
type llmModelJSON struct {
	Downloaded bool `json:"downloaded"`
}

func settingsJSON(cfgStore *settings.Store, models []asr.ModelSpec, modelsBaseDir string) (settingsData, modelsData, llmData []byte) {
	modelsJSON := make([]modelInfoJSON, 0, len(models))
	for _, m := range models {
		modelsJSON = append(modelsJSON, modelInfoJSON{
			Family:            m.Family,
			Variant:           m.Variant,
			Downloaded:        asr.IsDownloaded(modelsBaseDir, m),
			SupportsLanguage:  m.SupportsLanguage,
			SupportsStreaming: m.SupportsStreaming,
			Description:       m.Description,
		})
	}
	settingsData, _ = json.Marshal(cfgStore.Get())
	modelsData, _ = json.Marshal(modelsJSON)
	llmData, _ = json.Marshal(llmModelJSON{Downloaded: asr.IsDownloaded(modelsBaseDir, llm.Spec)})
	return settingsData, modelsData, llmData
}

// buildMainPage assembles the window from its parts: the shared kit, the
// settings screen's own stylesheet and markup, and the indicator component
// the settings preview draws.
func buildMainPage() string {
	page, err := assets.ReadFile("assets/main.html")
	if err != nil {
		panic(err)
	}
	out := injectAsset(string(page), kitCSSMarker, "kit.css")
	out = injectAsset(out, kitJSMarker, "kit.js")
	out = injectAsset(out, settingsCSSMarker, "settings.css")
	out = injectAsset(out, indicatorCSSMarker, "indicator.css")
	out = injectAsset(out, settingsMarkup, "settings-pane.html")
	return injectAsset(out, welcomeMarkup, "welcome.html")
}

// runMainWindow builds the window and its bindings. Runs inside a runOnMain
// closure -- see ShowMainWindow -- so every AppKit call in here, including the
// two on the first two lines, is already on the main thread.
func runMainWindow(pane string, store *history.Store, meetings *history.MeetingStore, tasks *task.Store, cfgStore *settings.Store, models []asr.ModelSpec, modelsBaseDir, recordingsDir string, srv *pageServer) {
	// Before activateApp, which makes Voxlog itself the answer.
	rememberFrontmostApp()
	activateApp()
	w := webview.New(false)
	winMu.Lock()
	mainWin = w
	mainOpening = false
	winMu.Unlock()
	w.SetTitle("Voxlog")
	w.SetSize(mainWidth, mainHeight, webview.HintNone)
	keepAliveOnClose(w.Window())

	// Deliberately not hideOnDeactivate: this is the app's window, not the
	// popover it replaced. It stays where the user put it, including behind
	// whatever they clicked into next.

	// No close handling needed: keepAliveOnClose keeps this exact window
	// alive after the red X, so the singleton handle stays valid and the
	// next Show simply orders the same window back on screen. (An earlier
	// version tried to detect the close from JS via pagehide and drop the
	// singleton -- WebKit never fires it here, which is precisely why
	// reopening was broken.)

	// What confirming an entry does -- clicking its row, or selecting it with
	// the arrow keys and pressing Enter. Copy is the default because pasting
	// blind into whatever sits behind an open window would be worse than
	// doing nothing; the paste setting is not blind, it puts the window away
	// and hands focus back to the app the History window took it from
	// (remembered in ShowMainWindow) before typing anything.
	//
	// copyOnly is the Copy button, which means the clipboard whatever the
	// setting says -- a button reading "Copy" that pastes and closes the
	// window is a different button.
	w.Bind("applyEntry", func(text string, copyOnly bool) error {
		switch confirmMode(cfgStore.Get().HistoryClickAction, copyOnly) {
		case output.ModeNone:
			return nil
		case output.ModePaste:
			// Not inline: the window has to be off screen and the other app
			// frontmost before Cmd+V is worth sending, and neither happens
			// while this binding is still holding up the call that made it.
			go pasteEntryBack(text)
			return nil
		default:
			return output.Emit(text, output.ModeCopy)
		}
	})

	// pasteEntry is the other confirmation gesture: a row picked with the
	// arrow keys and confirmed with Enter. That is always a paste, whatever
	// HistoryClickAction says -- the setting answers what a *click* means,
	// and somebody who walked a list with the keyboard and pressed Enter
	// asked for the line to go back where they were typing.
	w.Bind("pasteEntry", func(text string) error {
		// Not inline, for the same reason applyEntry's paste isn't: the
		// window has to be off screen and the other app frontmost before
		// Cmd+V is worth sending.
		go pasteEntryBack(text)
		return nil
	})

	// transcribeEntry decodes a meeting recorded earlier. Returns immediately:
	// the work is queued behind whatever else is decoding, and the row updates
	// itself when the transcript lands (see RefreshMainWindowIfOpen).
	w.Bind("transcribeEntry", func(id string) error {
		at, err := time.Parse(time.RFC3339Nano, id)
		if err != nil {
			return err
		}
		winMu.Lock()
		fn := transcribeHandler
		winMu.Unlock()
		if fn == nil {
			return errors.New("transcription is not available")
		}
		return fn(at)
	})

	// meetingStatus backs the Overview banner's poll. No handler installed
	// (a window opened before startup finished) is not a fault -- it just
	// means no meeting is running yet, as far as this window can tell.
	w.Bind("meetingStatus", func() (map[string]any, error) {
		winMu.Lock()
		fn := meetingStatusHandler
		winMu.Unlock()
		if fn == nil {
			return map[string]any{"running": false, "seconds": 0.0, "muted": false}, nil
		}
		running, seconds, muted := fn()
		return map[string]any{"running": running, "seconds": seconds, "muted": muted}, nil
	})

	// toggleMeetingMute backs the banner's Mute button. It answers with the
	// state after the flip rather than letting the page assume it: the menu
	// bar item toggles the same flag, so the page's idea of it can be stale
	// by the time the click lands.
	w.Bind("toggleMeetingMute", func() (bool, error) {
		winMu.Lock()
		fn := toggleMuteHandler
		winMu.Unlock()
		if fn == nil {
			return false, nil
		}
		return fn(), nil
	})

	// logJS is the page's only way to report a fault. This webview has no
	// console and no inspector: a TypeError in a click handler stops that
	// handler and leaves no trace at all, which is exactly how a row of
	// buttons can look wired and do nothing. Everything the page throws goes
	// to the same log file the rest of the app writes to.
	w.Bind("logJS", func(msg string) error {
		log.Printf("page: %s", msg)
		return nil
	})

	// stopMeeting backs the banner's Stop button -- the same method the tray
	// menu's "Stop meeting recording" item and the meeting hotkey call,
	// reached the same way transcribeEntry reaches into package main.
	w.Bind("stopMeeting", func() error {
		winMu.Lock()
		fn := stopMeetingHandler
		winMu.Unlock()
		if fn != nil {
			fn()
		}
		return nil
	})

	w.Bind("saveSettings", func(v settings.Settings) error {
		v = keepAppOwned(v, cfgStore.Get())
		if err := cfgStore.Set(v); err != nil {
			return err
		}
		// Tell the app. Most settings are read when they are next needed, so
		// nothing had to know before -- but a switch that starts and stops a
		// listener cannot wait to be noticed.
		settingsApplied(v)
		return nil
	})

	// finishWelcome records that the first-run welcome was finished or
	// dismissed, so the next launch opens on the menu bar alone.
	w.Bind("finishWelcome", func() error {
		v := cfgStore.Get()
		v.WelcomeDone = true
		v.WelcomeStep = 0
		return cfgStore.Set(v)
	})
	w.Bind("welcomeStep", func(step int) error {
		v := cfgStore.Get()
		v.WelcomeStep = step
		return cfgStore.Set(v)
	})

	// The API key never travels back to the window: it goes in, and after
	// that the pane only ever learns whether one is stored. A field that can
	// be read back is a field that ends up in a screenshot.
	w.Bind("setLLMAPIKey", func(key string) error { return llmKeySet(key) })
	w.Bind("llmAPIKeyStored", func() (bool, error) { return llmKeyStored(), nil })

	// The OpenRouter accounts. A key goes straight to the keychain under its
	// account's ID (an empty key removes it, which is how Remove cleans up);
	// the pane only ever asks which accounts have one.
	w.Bind("setOpenRouterKey", func(id, key string) error { return orKeySet(id, key) })
	w.Bind("openRouterKeysStored", func(ids []string) (map[string]bool, error) { return orKeysStored(ids), nil })
	w.Bind("openRouterFreeModels", func() ([]llm.ModelInfo, error) { return orFreeModels() })

	// testLLMConnection sends one tiny prompt to whatever the pane currently
	// shows, unsaved values included -- the point is to try a key or an
	// address before committing to it. Returns the provider's own words on
	// failure: "401 Unauthorized" and "no route to host" need different
	// fixes.
	w.Bind("testLLMConnection", func(v settings.Settings) (string, error) {
		if err := llmTest(v); err != nil {
			return err.Error(), nil
		}
		return "", nil
	})

	// notificationState is what the Listening pane needs to say whether the
	// recording notice will actually appear. macOS refusing the permission
	// is silent everywhere else in the app, and a courtesy banner nobody
	// sees is worse than no feature at all.
	w.Bind("notificationState", func() (map[string]any, error) {
		st := usernotify.Status()
		return map[string]any{
			"decided":   st.Decided,
			"granted":   st.Granted,
			"fell_back": st.FellBack,
		}, nil
	})

	// ...and the way to fix it, since the pane cannot grant the permission
	// itself: macOS only asks once, and after that it is System Settings or
	// nothing.
	w.Bind("openNotificationSettings", func() error {
		return exec.Command("open", "x-apple.systempreferences:com.apple.preference.notifications").Run()
	})

	// revealModels opens the models directory in Finder, so downloaded
	// models can be inspected or deleted without hunting through
	// ~/Library/Application Support by hand.
	w.Bind("revealModels", func() error {
		if err := os.MkdirAll(modelsBaseDir, 0o755); err != nil {
			return err
		}
		return exec.Command("open", modelsBaseDir).Run()
	})

	w.Bind("defaultTranscriptsDir", func() (string, error) {
		return history.DefaultDir(), nil
	})

	// chooseDirectory opens macOS's own folder picker. osascript rather
	// than a native NSOpenPanel: the panel would have to be driven on the
	// main thread around this binding's own main-thread call, and a
	// subprocess sidesteps that entirely for a control used once in a blue
	// moon. Returns "" when the user cancels.
	w.Bind("chooseDirectory", func() (string, error) {
		out, err := exec.Command("osascript", "-e",
			`POSIX path of (choose folder with prompt "Choose where to store transcripts")`).Output()
		if err != nil {
			return "", nil // cancelled (osascript exits non-zero) -- not an error worth surfacing
		}
		return strings.TrimRight(strings.TrimSpace(string(out)), "/"), nil
	})

	// restartApp relaunches Voxlog. Needed because macOS caches the answers
	// to AXIsProcessTrusted / CGPreflightScreenCaptureAccess for the life of
	// a process: once Accessibility or Screen Recording is granted, the
	// running app keeps seeing the old "denied" until it starts again.
	w.Bind("restartApp", func() error {
		target, err := relaunchTarget()
		if err != nil {
			return err
		}
		// Hand the replacement OpenSettingsFlag so it comes back up on this
		// same pane -- a restart prompted from Settings that dumps the user
		// back to a bare menu bar icon is a dead end. An error here goes back
		// to the page, which says so, rather than quitting into nothing.
		log.Printf("restart: relaunching %s", target)
		if err := startRelaunch(target, OpenSettingsFlag); err != nil {
			log.Printf("restart: %v", err)
			return err
		}
		go func() {
			// Let the binding's answer reach the page before going.
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}()
		return nil
	})

	w.Bind("inputDevices", func() ([]string, error) {
		names, err := audio.InputDevices()
		if err != nil {
			log.Printf("list input devices: %v", err)
			return []string{}, nil // an empty list still leaves "System default" usable
		}
		return names, nil
	})

	// keyLabel turns the stored "vk:54" form into "Right Command" for
	// display; the page keeps the raw id and only shows the label.
	w.Bind("keyLabel", func(raw string) (string, error) {
		return hotkey.ParseBinding(raw).Label(), nil
	})

	var mic micTest
	var compare modelComparison
	// micVolume/setMicVolume drive the INPUT DEVICE's own volume, the one
	// System Settings shows under Sound > Input. That is the only control
	// that changes how much signal the microphone actually produces;
	// multiplying the samples afterwards scales the noise along with the
	// voice and cannot recover what the converter never captured. A machine
	// found sitting at 8/100 records almost nothing, and no slider inside
	// this app could have fixed that.
	w.Bind("micVolume", func() (map[string]any, error) {
		value, ok := audio.InputVolume()
		return map[string]any{
			"value":    value,
			"readable": ok,
			"settable": audio.InputVolumeSettable(),
		}, nil
	})
	w.Bind("setMicVolume", func(v float64) (bool, error) {
		return audio.SetInputVolume(v), nil
	})

	// The model comparison: record once, then transcribe that same recording
	// with every downloaded model and report what each made of it and how
	// long it took. Nothing is written to history -- this is a measurement,
	// not a dictation, and filling the log with four copies of the same
	// sentence would be its own bug.
	w.Bind("startModelTest", func() error {
		return compare.start(cfgStore.Get().InputDevice, cfgStore.Get().MicGain)
	})
	w.Bind("runModelTest", func() ([]map[string]any, error) {
		return compare.finish(models, modelsBaseDir, cfgStore.Get().Language)
	})
	w.Bind("cancelModelTest", func() error {
		compare.cancel()
		return nil
	})

	w.Bind("startMicTest", func(device string, gain float64) error {
		return mic.start(device, gain, func(level float64) {
			runOnMain(func() {
				w.Eval(fmt.Sprintf("window.voxlog.onMicLevel && window.voxlog.onMicLevel(%.3f)", level))
			})
		})
	})
	w.Bind("stopMicTest", func() error {
		mic.stop()
		return nil
	})

	w.Bind("checkPermissions", func() (map[string]string, error) {
		access, screen := cachedStaticPermissions()
		return map[string]string{
			"accessibility": access,
			// The only one of the three that can change under a running
			// process, so the only one asked again on every poll.
			"microphone":      string(permissions.Microphone()),
			"screenrecording": screen,
			// "" when the tap is working or has not been asked to run.
			"systemaudio": systemAudioError(),
		}, nil
	})

	// Kept as a JS-callable hook purely to stop an in-flight mic test when
	// the page goes away; the window itself survives its close button (see
	// keepAliveOnClose) so the singleton is deliberately left intact.
	w.Bind("settingsWindowClosed", func() error {
		mic.stop()
		return nil
	})

	// requestPermission triggers the OS's own native grant dialog for the
	// named permission and returns immediately -- it doesn't wait for the
	// user to respond (there's nothing to wait for: Accessibility only
	// updates after a relaunch, and the mic prompt is itself async). The
	// caller re-runs checkPermissions afterward to see the result.
	w.Bind("requestPermission", func(name string) error {
		switch name {
		case "accessibility":
			permissions.RequestAccessibility()
		case "microphone":
			go permissions.RequestMicrophone() // opens the mic briefly; don't block the UI on it
		case "screenrecording":
			permissions.RequestScreenRecording()
		}
		return nil
	})

	// captureKey returns immediately (waiting on a real keypress can take
	// several seconds, and Bind handlers run on the main thread -- blocking
	// here would freeze the whole app's UI, not just this window, for
	// however long the user takes to press a key). The actual capture runs
	// in a goroutine; its result is pushed to JS via onKeyCaptured once
	// ready, the same async-result pattern downloadModel already uses below.
	w.Bind("captureKey", func() error {
		go func() {
			binding, err := hotkey.CaptureNextKey(10 * time.Second)
			payload := map[string]string{}
			if err != nil {
				payload["error"] = err.Error()
			} else {
				payload["keyId"] = binding.String()
			}
			data, _ := json.Marshal(payload)
			runOnMain(func() {
				w.Eval(fmt.Sprintf("window.voxlog.onKeyCaptured && window.voxlog.onKeyCaptured(%s)", data))
			})
		}()
		return nil
	})

	w.Bind("downloadModel", func(family, variant string) error {
		var spec *asr.ModelSpec
		for i := range models {
			if models[i].Family == family && models[i].Variant == variant {
				spec = &models[i]
				break
			}
		}
		if spec == nil {
			return fmt.Errorf("unknown model %s/%s", family, variant)
		}

		go func(spec asr.ModelSpec) {
			err := asr.Download(modelsBaseDir, spec, func(file string, downloaded, total int64) {
				payload, _ := json.Marshal(downloadProgressJSON{
					FileFamily:  spec.Family,
					FileVariant: spec.Variant,
					Downloaded:  downloaded,
					Total:       total,
				})
				runOnMain(func() {
					w.Eval(fmt.Sprintf("window.voxlog.onProgress && window.voxlog.onProgress(%s)", payload))
				})
			}, httpFetch)

			done := downloadDoneJSON{Family: spec.Family, Variant: spec.Variant}
			if err != nil {
				done.Error = err.Error()
			}
			payload, _ := json.Marshal(done)
			runOnMain(func() {
				w.Eval(fmt.Sprintf("window.voxlog.onDownloadDone && window.voxlog.onDownloadDone(%s)", payload))
			})
		}(*spec)

		return nil
	})

	// recordingsSize reports what the folder Settings' Free-up-space row and
	// each row's Delete-audio action both act on currently occupies. Bytes
	// travel alongside the human string so the page can compare sizes
	// without parsing "4.6 GB" back apart.
	w.Bind("recordingsSize", func() (map[string]any, error) {
		n, err := history.RecordingsSize(recordingsDir)
		if err != nil {
			return nil, err
		}
		return map[string]any{"bytes": n, "human": humanBytes(n)}, nil
	})

	// freeUpSpace runs the same sweep startup and post-recording already
	// trigger, on demand, against whatever AudioRetention/AudioMaxGB say
	// right now -- never a stricter pass than the settings the user can see,
	// which is what sweepHandler (== app.sweepRecordings) already enforces.
	w.Bind("freeUpSpace", func() error {
		winMu.Lock()
		fn := sweepHandler
		winMu.Unlock()
		if fn != nil {
			fn()
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// deleteRecording removes one file the user picked by hand. Reachable
	// from the page, so the same guard serveAudio uses (server.go) applies
	// here for the same reason: filepath.Base(name) equal to name is the one
	// check that rules out "..", a nested path, and an absolute path all at
	// once, before the name ever reaches a Join.
	w.Bind("deleteRecording", func(name string) error {
		if name == "" || name != filepath.Base(name) {
			return errors.New("invalid recording name")
		}
		if err := os.Remove(filepath.Join(recordingsDir, name)); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// Everything the meeting screen and the Voices pane need -- opening one
	// meeting, filing it under a project, and naming the voices in it.
	bindMeetings(w, store, meetings, tasks)
	bindMCP(w)

	// setTaskStatus backs the Tasks pane's status control -- cycling a task
	// through To do/In progress/Blocked/Done. Marking a task Done cancels its
	// reminder, if it still had one pending.
	w.Bind("setTaskStatus", func(id, status string) error {
		var next task.Status
		switch status {
		case string(task.StatusTodo), string(task.StatusInProgress), string(task.StatusBlocked), string(task.StatusDone):
			next = task.Status(status)
		default:
			return fmt.Errorf("unknown task status %q", status)
		}
		if err := tasks.Update(id, func(t *task.Task) { t.Status = next }); err != nil {
			return err
		}
		if next == task.StatusDone {
			task.CancelReminder(id)
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// setTaskNotes and setTaskText back the task detail view. Both stamp
	// Updated: once a line has been edited by hand, "when did I last touch
	// this" is a different question from "when was it classified".
	//
	// setTaskNotes deliberately does NOT refresh the window: it is called on
	// a debounce while the user is still typing, and a refresh redraws the
	// Tasks pane -- replacing the textarea under the caret. The page keeps
	// its own copy of what was typed; the next refresh from anything else
	// picks the notes up from disk.
	w.Bind("setTaskNotes", func(id, notes string) error {
		return tasks.Update(id, func(t *task.Task) {
			t.Notes = notes
			t.Updated = time.Now()
		})
	})

	// setTaskText edits the extracted line itself -- the classifier's wording
	// is often nearly-but-not-quite right, and rejecting the whole task over
	// a wrong verb is too blunt. Empty text is refused: it would leave an
	// unidentifiable row.
	w.Bind("setTaskText", func(id, text string) error {
		text = strings.TrimSpace(text)
		if text == "" {
			return fmt.Errorf("task text cannot be empty")
		}
		if err := tasks.Update(id, func(t *task.Task) {
			t.Text = text
			t.Updated = time.Now()
		}); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// setTaskEntity files a task under a project by hand. The classifier is
	// the only thing that used to set this, so a task it filed wrong (or left
	// in Unfiltered because nothing matched) could not be moved without
	// editing the settings dictionary and waiting for a reclassification.
	// Free text rather than a checked list, like setMeetingEntity: the page
	// offers the dictionary, and a project typed nowhere else still groups.
	w.Bind("setTaskEntity", func(id, entity string) error {
		entity = strings.TrimSpace(entity)
		if err := tasks.Update(id, func(t *task.Task) {
			t.Entity = entity
			t.Updated = time.Now()
		}); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// setTaskReminder sets, moves or clears a task's reminder. An empty
	// string clears it; anything else is RFC3339 from the page, which is
	// what a <input type="datetime-local"> read back as a Date serializes to.
	//
	// The timer is re-armed here, not only at startup: task.ScheduleReminder
	// replaces any timer already running for this id, and CancelReminder
	// first covers the clear-it case and a reminder moved into the past.
	// A time already gone is deliberately not armed -- it has nothing left to
	// wait for, and the pane draws it as overdue.
	w.Bind("setTaskReminder", func(id, when string) error {
		var at *time.Time
		if when = strings.TrimSpace(when); when != "" {
			parsed, err := time.Parse(time.RFC3339, when)
			if err != nil {
				return fmt.Errorf("reminder %q is not a time: %w", when, err)
			}
			at = &parsed
		}
		if err := tasks.Update(id, func(t *task.Task) {
			t.Reminder = at
			t.Updated = time.Now()
		}); err != nil {
			return err
		}
		task.CancelReminder(id)
		if at != nil {
			if updated, err := tasks.Get(id); err == nil {
				task.ScheduleReminder(updated)
			} else {
				log.Printf("task: re-arming reminder for %s: %v", id, err)
			}
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// removeRejected forgets a "Not a task" correction: the entry stops being
	// a negative example for the classifier and disappears from the list.
	w.Bind("removeRejected", func(text string) error {
		if err := tasks.RemoveRejected(text); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// restoreRejected is "actually a task after all": drop the negative
	// example and put the line back in Tasks as a fresh todo. The original
	// source is not recoverable -- rejection only ever stored the text -- so
	// the restored task has no SourceKind/SourceKey and links nowhere.
	w.Bind("restoreRejected", func(text string) error {
		if err := tasks.RemoveRejected(text); err != nil {
			return err
		}
		if err := tasks.Append(task.Task{
			ID:      task.NewID(),
			Text:    text,
			Status:  task.StatusTodo,
			Created: time.Now(),
		}); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// deleteTask removes a task with no further signal -- the neutral case
	// (a duplicate, already handled another way, no longer relevant). Kept
	// separate from rejectTask below on purpose: a plain cleanup deletion is
	// not evidence the classifier was wrong, and folding the two together
	// would feed the "wrong" list with tasks that were never actually
	// misclassified.
	w.Bind("deleteTask", func(id string) error {
		if err := tasks.Delete(id); err != nil {
			return err
		}
		task.CancelReminder(id)
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// rejectTask is "Not a task" -- an explicit correction, not just tidying
	// up. Records the extracted text as a negative example (see
	// task.Store.AppendRejected / llm.Classify's rejected param) before
	// deleting, so future classification passes are nudged away from
	// repeating it.
	w.Bind("rejectTask", func(id string) error {
		t, err := tasks.Get(id)
		if err != nil {
			return err
		}
		if err := tasks.AppendRejected(t.Text); err != nil {
			log.Printf("task reject: recording example: %v", err)
		}
		if err := tasks.Delete(id); err != nil {
			return err
		}
		task.CancelReminder(id)
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// downloadLLMModel provisions everything Task Hub's classifier needs: the
	// Python + mlx-lm runtime (built on this machine, not shipped in the app
	// -- see llm.EnsureRuntime) and then the model itself, via the same
	// asr.Download -> progress-callback -> w.Eval -> JS-callback shape as
	// downloadModel above, with its own window.voxlog.onLLM* names so the two
	// downloads' progress bars never collide.
	w.Bind("downloadLLMModel", func() error {
		go func() {
			if err := llm.EnsureRuntime(modelsBaseDir, func(stage string) {
				payload, _ := json.Marshal(map[string]string{"stage": stage})
				runOnMain(func() {
					w.Eval(fmt.Sprintf("window.voxlog.onLLMStage && window.voxlog.onLLMStage(%s)", payload))
				})
			}); err != nil {
				payload, _ := json.Marshal(downloadDoneJSON{Family: llm.Spec.Family, Variant: llm.Spec.Variant, Error: err.Error()})
				runOnMain(func() {
					w.Eval(fmt.Sprintf("window.voxlog.onLLMDownloadDone && window.voxlog.onLLMDownloadDone(%s)", payload))
				})
				return
			}
			payload, _ := json.Marshal(map[string]string{"stage": "Downloading model…"})
			runOnMain(func() {
				w.Eval(fmt.Sprintf("window.voxlog.onLLMStage && window.voxlog.onLLMStage(%s)", payload))
			})

			err := asr.Download(modelsBaseDir, llm.Spec, func(file string, downloaded, total int64) {
				payload, _ := json.Marshal(downloadProgressJSON{
					FileFamily:  llm.Spec.Family,
					FileVariant: llm.Spec.Variant,
					Downloaded:  downloaded,
					Total:       total,
				})
				runOnMain(func() {
					w.Eval(fmt.Sprintf("window.voxlog.onLLMProgress && window.voxlog.onLLMProgress(%s)", payload))
				})
			}, httpFetch)

			done := downloadDoneJSON{Family: llm.Spec.Family, Variant: llm.Spec.Variant}
			if err != nil {
				done.Error = err.Error()
			}
			payload, _ = json.Marshal(done)
			runOnMain(func() {
				w.Eval(fmt.Sprintf("window.voxlog.onLLMDownloadDone && window.voxlog.onLLMDownloadDone(%s)", payload))
			})
		}()
		return nil
	})

	daysData, meetingsData, overviewData, tasksData, rejectedData := pageData(store, meetings, tasks)
	settingsData, modelsData, llmData := settingsJSON(cfgStore, models, modelsBaseDir)
	// Init runs at document start on every navigation (including the one
	// below), so window.voxlog is populated before the page's own script
	// runs, whether the page was just built or is a reload.
	w.Init(fmt.Sprintf(
		"window.voxlog = window.voxlog || {}; window.voxlog.pane = %q; window.voxlog.days = %s; window.voxlog.meetings = %s; window.voxlog.overview = %s; window.voxlog.tasks = %s; window.voxlog.rejected = %s; window.voxlog.decodeQueue = %s; window.voxlog.settings = %s; window.voxlog.models = %s; window.voxlog.llmModel = %s; window.voxlog.audioBase = %q; window.voxlog.voiceBase = %q; window.voxlog.mixBase = %q;",
		pane, daysData, meetingsData, overviewData, tasksData, rejectedData, decodeQueueJSON(), settingsData, modelsData, llmData, srv.AudioURL(), srv.VoiceURL(), srv.MixURL(),
	))

	// Navigate, not SetHtml: the page is served over loopback (see
	// startPageServer) so it can fetch recordings by name from the same
	// origin without smuggling an absolute filesystem path into the page.
	w.Navigate(srv.PageURL())
}
