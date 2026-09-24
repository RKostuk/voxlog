package main

import (
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/history"
	"voxlog-go/internal/keychain"
	"voxlog-go/internal/llm"
	"voxlog-go/internal/mcp"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/task"
	"voxlog-go/internal/ui"
)

// app is everything a hotkey press needs to do its job, in one place.
//
// It exists because there are now two recordings that can be live at the same
// time -- a meeting and a dictation inside it -- and their state cannot be a
// handful of variables closed over by one function the way a single take's
// was.
type app struct {
	store     *settings.Store
	hist      *history.Store
	meetings  *history.MeetingStore
	tasks     *task.Store
	llm       *llm.Cache
	modelsDir string
	overlay   *ui.Overlay
	models    *transcriberCache
	speakers  *diarizerCache
	embedders *embedderCache
	queue     *decodeQueue
	tray      *tray
	// listen is always-on listening: the microphone gate that starts a
	// recording by itself when it hears a conversation (see alwayson.go).
	listen alwaysOn

	// mu guards the two session slots, and nothing else. It is held for as
	// long as it takes to open or close a capture and never across a decode
	// or an output.Emit -- which is the whole difference from the single lock
	// this replaces, where every hotkey press waited for the previous take's
	// transcript.
	mu        sync.Mutex
	dictation *dictation
	meeting   *meeting
	// backfillStop unwinds a running re-read of an old meeting. Closed when
	// a recording starts, re-made when the next re-read asks for it.
	backfillStop chan struct{}

	// onMeetingState shows or hides the tray's "Stop meeting recording" item.
	// A menu entry that does nothing most of the time is worse than no entry.
	onMeetingState func(running bool)

	// The MCP server, and whether it is up. Its own lock: it is started and
	// stopped from the settings pane, which has nothing to do with the
	// recording slots mu guards. Nil when the feature is off -- that is the
	// whole of what a disabled server costs.
	mcpMu  sync.Mutex
	mcpSrv *mcp.Server
	mcpErr string
}

func newApp(store *settings.Store, hist *history.Store, meetings *history.MeetingStore, tasks *task.Store, modelsDir string, overlay *ui.Overlay) *app {
	a := &app{
		store:     store,
		hist:      hist,
		meetings:  meetings,
		tasks:     tasks,
		llm:       llm.NewCache(modelsDir),
		modelsDir: modelsDir,
		overlay:   overlay,
		models:    &transcriberCache{},
		speakers:  &diarizerCache{},
		embedders: &embedderCache{},
		tray:      &tray{},
	}
	// One queue, two consumers: the menu bar only wants a count, the main
	// window wants the labels so it can say which recording is decoding.
	a.queue = newDecodeQueue(func(list []decodeStatus) {
		a.tray.setDecoding(len(list))
		out := make([]ui.DecodeStatus, 0, len(list))
		for _, st := range list {
			out = append(out, ui.DecodeStatus{
				Label:      st.Label,
				Kind:       st.Kind,
				Stage:      st.Stage,
				Key:        st.Key,
				Seconds:    st.Seconds,
				Running:    st.Running,
				QueuedAtMS: st.QueuedAtMS,
			})
		}
		ui.PublishDecodeQueue(append(out, currentLLMJobs()...))
	})
	// The LLM jobs republish through the same path, so a summary starting
	// while nothing is decoding still reaches the window.
	llmJobMu.Lock()
	llmPublish = func() { a.queue.republish() }
	llmJobMu.Unlock()
	return a
}

// LLM work runs as fire-and-forget goroutines, outside the decode queue, and
// until now reported nothing at all -- so a machine busy summarising a
// meeting looked idle. These two functions publish those jobs into the same
// list the transcription queue feeds, which is what makes "what is Voxlog
// doing right now" one question with one answer.
var (
	llmJobMu   sync.Mutex
	llmJobs    []ui.DecodeStatus
	llmPublish func()
)

// trackLLM adds a job to the list and returns the function that removes it.
// Always call the returned function, deferred: a job that never clears is a
// spinner that never stops.
func trackLLM(label, stage string) func() {
	job := ui.DecodeStatus{
		Label:      label,
		Kind:       "llm",
		Stage:      stage,
		Running:    true,
		QueuedAtMS: time.Now().UnixMilli(),
	}
	llmJobMu.Lock()
	llmJobs = append(llmJobs, job)
	publish := llmPublish
	llmJobMu.Unlock()
	if publish != nil {
		publish()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			llmJobMu.Lock()
			for i := range llmJobs {
				if llmJobs[i].QueuedAtMS == job.QueuedAtMS && llmJobs[i].Label == job.Label {
					llmJobs = append(llmJobs[:i], llmJobs[i+1:]...)
					break
				}
			}
			publish := llmPublish
			llmJobMu.Unlock()
			if publish != nil {
				publish()
			}
		})
	}
}

func currentLLMJobs() []ui.DecodeStatus {
	llmJobMu.Lock()
	defer llmJobMu.Unlock()
	return append([]ui.DecodeStatus(nil), llmJobs...)
}

// llmEndpoint is where this app's prompts go: the zero Endpoint for the
// downloaded model (llm starts and owns that server itself), or the API the
// user configured. The key is read here, per call, rather than held in
// memory for the life of the process -- it changes in Settings, and the
// keychain is the one copy of it.
func (a *app) llmEndpoint(cfg settings.Settings) llm.Endpoint {
	if cfg.LLMProvider != settings.LLMProviderAPI || strings.TrimSpace(cfg.LLMBaseURL) == "" {
		return llm.Endpoint{}
	}
	key, err := keychain.Get(keychain.LLMService, keychain.LLMAccount)
	if err != nil && !errors.Is(err, keychain.ErrNotFound) {
		// Not fatal: an endpoint on the local network may want no key at all,
		// and a provider that does will say so itself in the reply.
		log.Printf("llm: reading the API key: %v", err)
	}
	return llm.Endpoint{
		BaseURL: cfg.LLMBaseURL,
		Model:   cfg.LLMModel,
		APIKey:  key,
	}
}

// llmReady reports whether there is a model to ask at all. With an API
// configured there is nothing to download -- requiring the 4.5GB local model
// anyway, as this used to, would mean a remote-only setup silently did
// nothing.
func (a *app) llmReady(cfg settings.Settings) bool {
	if a.llmEndpoint(cfg).Remote() {
		return true
	}
	return asr.IsDownloaded(a.modelsDir, llm.Spec)
}

// classifyForTasks runs Task Hub's classifier over a finished transcript in
// the background. Called as a goroutine right after the dictation/meeting
// store write it's tagging succeeds (see dictate.go/meeting.go) -- never on
// the hot path, and never surfaced to the user as an error: a missed
// classification is a background feature quietly doing nothing, not a
// failure worth a banner.
func (a *app) classifyForTasks(sourceKind, sourceKey, text string) {
	cfg := a.store.Get()
	if !cfg.TaskHubEnabled || !a.llmReady(cfg) {
		return
	}
	label := "Dictation"
	if sourceKind == history.KindMeeting {
		label = "Meeting"
	}
	defer trackLLM(label, "Finding tasks")()

	entities := a.entityNames(cfg)
	rejected, err := a.tasks.LoadRejected()
	if err != nil {
		log.Printf("task classify: rejected examples: %v", err)
	}
	modelDir := asr.ModelDir(a.modelsDir, llm.Spec)
	result, err := a.llm.Classify(modelDir, text, entities, rejected, time.Now(), a.llmEndpoint(cfg))
	if err != nil {
		log.Printf("task classify: %v", err)
		return
	}
	if !result.IsTask {
		return
	}

	t := task.Task{
		ID:         task.NewID(),
		SourceKind: sourceKind,
		SourceKey:  sourceKey,
		Text:       result.Text,
		Entity:     a.taskEntity(sourceKind, sourceKey, result.Entity),
		Status:     task.Status(result.Status),
		Reminder:   result.Reminder,
		Created:    time.Now(),
	}
	if err := a.tasks.Append(t); err != nil {
		log.Printf("task append: %v", err)
		return
	}
	if t.Reminder != nil {
		task.ScheduleReminder(t)
	}
	ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
}

// taskEntity is the project a task from this transcript belongs under. The
// classifier's own answer wins; a task out of a meeting that the classifier
// could not place inherits the meeting's project instead of landing in
// Unfiltered, because the meeting has already been placed -- by the user, or
// by summarization.
//
// Read here, at write time, rather than passed in: summarization and
// classification are separate goroutines over the same transcript (see
// meeting.go), so the meeting's own project may only have been decided
// seconds ago. If it has not been decided yet, this is Unfiltered and stays
// Unfiltered -- the two passes are deliberately not serialized for the sake
// of one field.
func (a *app) taskEntity(sourceKind, sourceKey, classified string) string {
	if classified != "" && classified != llm.UnfilteredEntity {
		return classified
	}
	if sourceKind != history.KindMeeting {
		return classified
	}
	start, err := time.Parse(time.RFC3339Nano, sourceKey)
	if err != nil {
		return classified
	}
	m, err := a.meetings.Get(start)
	if err != nil || m.Entity == "" {
		return classified
	}
	return m.Entity
}

// summarizeMeeting fills in a meeting's Summary after its transcript lands --
// same trigger, same model, and the same "never surfaced as an error"
// philosophy as classifyForTasks, just a different prompt. Runs as its own
// goroutine from transcribeMeetingEntry (meeting.go), independent of task
// classification, so one failing doesn't hold up the other.
func (a *app) summarizeMeeting(start time.Time, text string) {
	cfg := a.store.Get()
	if !cfg.TaskHubEnabled || !cfg.SummaryEnabled || !a.llmReady(cfg) {
		return
	}
	defer trackLLM("Meeting, "+start.Format("15:04"), "Summarising")()

	entities := a.entityNames(cfg)
	modelDir := asr.ModelDir(a.modelsDir, llm.Spec)
	title, summary, entity, err := a.llm.Summarize(modelDir, text, entities, llm.SummaryOptions{
		Length: cfg.SummaryLength,
		Extra:  cfg.SummaryPromptExtra,
	}, a.llmEndpoint(cfg))
	if err != nil {
		log.Printf("meeting summarize: %v", err)
		return
	}
	// Each of the three answers is worth storing even when the other two came
	// back empty: they are three answers, and one of them failing is not a
	// reason to drop the rest.
	if err := a.meetings.SetTitleAuto(start, title); err != nil {
		log.Printf("meeting summarize: naming it %q: %v", title, err)
	}
	if entity != "" {
		if _, err := a.meetings.SetEntityAuto(start, entity); err != nil {
			log.Printf("meeting summarize: filing under %q: %v", entity, err)
		}
	}
	if summary == "" {
		if entity != "" || title != "" {
			ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
		}
		return
	}
	if err := a.meetings.Update(start, func(m *history.Meeting) { m.Summary = summary }); err != nil {
		log.Printf("meeting summarize: attaching summary: %v", err)
		return
	}
	ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
}

// entityNames is the closed list of projects Task Hub is allowed to file a
// task under -- the user's own dictionary (Settings > LLM model), nothing
// else. The model no longer gets to invent new project names (see
// llm.normalizeEntity, which enforces this even if the model tries anyway);
// anything not on this list, including everything when the list is empty,
// lands under llm.UnfilteredEntity.
func (a *app) entityNames(cfg settings.Settings) []string {
	seen := make(map[string]bool, len(cfg.EntityDictionary))
	names := make([]string, 0, len(cfg.EntityDictionary))
	for _, n := range cfg.EntityDictionary {
		key := strings.ToLower(n)
		if n == "" || seen[key] {
			continue
		}
		seen[key] = true
		names = append(names, n)
	}
	return names
}

// model resolves and checks the configured model, notifying the user about
// the two ways it can be unusable. Deliberately cheap -- a slice scan and a
// few os.Stat calls -- so a misconfigured model is reported before the
// microphone is touched, while the expensive load happens elsewhere.
func (a *app) model(cfg settings.Settings) (asr.ModelSpec, bool) {
	spec, ok := modelForSettings(cfg)
	if !ok {
		log.Printf("no known model for %s/%s", cfg.ModelFamily, cfg.ModelVariant)
		notify("No model selected. Pick one in Settings.")
		a.openSettingsForMissingModel()
		return spec, false
	}
	if !asr.IsDownloaded(a.modelsDir, spec) {
		log.Printf("model %s/%s not downloaded", spec.Family, spec.Variant)
		notify("Model not downloaded yet. Download it in Settings first.")
		a.openSettingsForMissingModel()
		return spec, false
	}
	return spec, true
}

// openSettingsForMissingModel opens the Settings window directly, on top of
// the notify() calls above. A hotkey press that can't record anything must
// never end in "nothing but a log line" -- the notification alone depends
// on Notification Center permission this app can't guarantee is granted (see
// internal/usernotify), but a window popping open cannot silently fail the
// same way.
func (a *app) openSettingsForMissingModel() {
	recordingsDir := filepath.Join(a.hist.Dir(), meetingsDirName)
	ui.ShowMainWindow("settings", a.hist, a.meetings, a.tasks, a.store, knownModels, a.modelsDir, recordingsDir)
}

// meetingRunning reports whether a meeting is recording right now.
func (a *app) meetingRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meeting != nil
}

// sweepRecordings applies the cleanup settings. Called at startup and after
// each recording finishes -- the folder only grows at those moments, so
// there is nothing for a timer to catch in between.
func (a *app) sweepRecordings() {
	cfg := a.store.Get()
	dir := filepath.Join(a.hist.Dir(), meetingsDirName)
	removed, freed, err := history.SweepRecordings(dir, cfg.AudioRetention, int64(cfg.AudioMaxGB*1e9), a.recordingsInUse())
	if err != nil {
		log.Printf("recordings sweep: %v", err)
		return
	}
	if removed > 0 {
		log.Printf("recordings sweep: removed %d file(s), freed %d bytes", removed, freed)
	}
}

// recordingsInUse is the set of WAVs a running meeting still has open. The
// sweep must not delete a file that is still being written to.
func (a *app) recordingsInUse() map[string]bool {
	a.mu.Lock()
	m := a.meeting
	a.mu.Unlock()

	inUse := map[string]bool{}
	if m != nil {
		if m.micPath != "" {
			inUse[m.micPath] = true
		}
		if m.sysPath != "" {
			inUse[m.sysPath] = true
		}
	}
	// Always-on's own recording counts too: it is being written to right
	// now, and it is the file a listening machine produces most of.
	if mic, sys := a.sessionPathsInUse(); mic != "" || sys != "" {
		if mic != "" {
			inUse[mic] = true
		}
		if sys != "" {
			inUse[sys] = true
		}
	}
	return inUse
}

// releaseOverlay puts the capsule away, unless a recording has since taken it
// over. A decode finishing must not hide the overlay of the take that started
// while it was running.
func (a *app) releaseOverlay() {
	a.mu.Lock()
	busy := a.dictation != nil
	a.mu.Unlock()
	if busy {
		return
	}
	a.overlay.SetTranscribing(false)
	a.overlay.ClearText()
	a.overlay.Hide()
}

// testLLM answers the settings pane's Test connection button for the values
// the user is looking at, saved or not. A local provider is checked the
// expensive way on purpose -- starting the server and loading the model is
// exactly what fails, and finding that out here beats finding it out from a
// meeting with no summary.
func (a *app) testLLM(cfg settings.Settings) error {
	ep := a.llmEndpoint(cfg)
	if !ep.Remote() {
		if !asr.IsDownloaded(a.modelsDir, llm.Spec) {
			return errors.New("the local model is not downloaded yet")
		}
		var err error
		ep, err = a.llm.LocalEndpoint(asr.ModelDir(a.modelsDir, llm.Spec))
		if err != nil {
			return err
		}
	}
	return llm.Ping(ep)
}
