package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/history"
	"voxlog-go/internal/llm"
	"voxlog-go/internal/mcp"
	"voxlog-go/internal/secrets"
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
	store    *settings.Store
	hist     *history.Store
	meetings *history.MeetingStore
	tasks    *task.Store
	llm      *llm.Cache
	// secrets holds the LLM providers' API keys (see internal/secrets).
	secrets   *secrets.Store
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

	// orLimits is when each OpenRouter account's free daily limit comes
	// back, by account ID, for the accounts that have hit it (see
	// noteLLMResult). In memory only: a restart forgets it, and the next
	// refused request learns it again.
	orLimitMu sync.Mutex
	orLimits  map[string]time.Time
	// orQuotas is the last free-request count read per account, and when:
	// the panes redraw often, and the count only moves with a request.
	orQuotas map[string]cachedQuota

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
		secrets:   secrets.Open(secrets.DefaultPath(settings.Path())),
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
// downloaded model (llm starts and owns that server itself), or the API or
// OpenRouter account the user configured. The key is read here, per call, rather than held in
// memory for the life of the process -- it changes in Settings, and the
// secrets file is the one copy of it.
func (a *app) llmEndpoint(cfg settings.Settings) llm.Endpoint {
	return endpointFor(cfg, a.secrets.Get)
}

// endpointFor is llmEndpoint with the key lookup passed in, so which key and
// which address a configuration ends up using can be tested without a file.
func endpointFor(cfg settings.Settings, getKey func(service, account string) (string, error)) llm.Endpoint {
	readKey := func(service, account string) string {
		key, err := getKey(service, account)
		if err != nil && !errors.Is(err, secrets.ErrNotFound) {
			// Not fatal: an endpoint on the local network may want no key at
			// all, and a provider that does will say so itself in the reply.
			log.Printf("llm: reading the API key: %v", err)
		}
		return key
	}
	switch cfg.LLMProvider {
	case settings.LLMProviderAPI:
		if strings.TrimSpace(cfg.LLMBaseURL) == "" {
			return llm.Endpoint{}
		}
		return llm.Endpoint{
			BaseURL: cfg.LLMBaseURL,
			Model:   cfg.LLMModel,
			APIKey:  readKey(secrets.LLMService, secrets.LLMAccount),
		}
	case settings.LLMProviderOpenRouter:
		// No model picked means nothing to ask: the zero endpoint keeps
		// llmReady honest instead of sending every prompt to a 400.
		if strings.TrimSpace(cfg.OpenRouterModel) == "" {
			return llm.Endpoint{}
		}
		fallbacks := cfg.OpenRouterFallbacks
		if len(fallbacks) > settings.MaxOpenRouterFallbacks {
			fallbacks = fallbacks[:settings.MaxOpenRouterFallbacks]
		}
		key := ""
		if cfg.OpenRouterActive != "" {
			key = readKey(secrets.OpenRouterService, cfg.OpenRouterActive)
		}
		return llm.Endpoint{
			BaseURL:   llm.OpenRouterBaseURL,
			Model:     cfg.OpenRouterModel,
			APIKey:    key,
			Fallbacks: fallbacks,
		}
	}
	return llm.Endpoint{}
}

// llmReady reports whether there is a model to ask at all. With an API
// configured there is nothing to download -- requiring the 4.5GB local model
// anyway, as this used to, would mean a remote-only setup silently did
// nothing.
func (a *app) llmReady(cfg settings.Settings) bool {
	if ep := a.llmEndpoint(cfg); ep.Remote() {
		// OpenRouter answers nothing without a key, and there is no point
		// sending every dictation off to be refused.
		return cfg.LLMProvider != settings.LLMProviderOpenRouter || strings.TrimSpace(ep.APIKey) != ""
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
	results, err := a.findTasks(sourceKind, text)
	if err != nil {
		log.Printf("task classify: %v", err)
		return
	}
	a.saveTasks(sourceKind, sourceKey, results)
}

// findTasks asks the LLM which tasks text holds. An error means the question
// could not be answered -- no model, no key, a rate limit -- which is not
// the same as the empty answer "no tasks".
func (a *app) findTasks(sourceKind, text string) ([]llm.Result, error) {
	cfg := a.store.Get()
	if !a.llmReady(cfg) {
		return nil, fmt.Errorf("no model is ready (provider %q)", cfg.LLMProvider)
	}
	label := "Dictation"
	if sourceKind == history.KindMeeting {
		label = "Meeting"
	}
	defer trackLLM(label, "Finding tasks")()

	rejected, err := a.tasks.LoadRejected()
	if err != nil {
		log.Printf("task classify: rejected examples: %v", err)
	}
	if err := a.limitedLLM(cfg); err != nil {
		return nil, err
	}
	modelDir := asr.ModelDir(a.modelsDir, llm.Spec)
	results, err := a.llm.Classify(modelDir, text, a.entityNames(cfg), rejected, cfg.TaskPromptExtra, time.Now(), a.llmEndpoint(cfg))
	a.noteLLMResult(cfg, err)
	return results, err
}

// saveTasks files what findTasks found under the transcript it came from.
func (a *app) saveTasks(sourceKind, sourceKey string, results []llm.Result) {
	if len(results) == 0 {
		return
	}
	for _, result := range results {
		t := task.Task{
			ID:         task.NewID(),
			SourceKind: sourceKind,
			SourceKey:  sourceKey,
			Text:       result.Text,
			Notes:      result.Notes,
			Entity:     a.taskEntity(sourceKind, sourceKey, result.Entity),
			Status:     task.Status(result.Status),
			Reminder:   result.Reminder,
			Created:    time.Now(),
		}
		if err := a.tasks.Append(t); err != nil {
			log.Printf("task append: %v", err)
			continue
		}
		if t.Reminder != nil {
			task.ScheduleReminder(t)
		}
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
	// Not gated on Task Hub. Summarizing was split out of it precisely
	// because finding tasks and naming a call are separate wants -- but the
	// gate here still demanded both, so a user with Task Hub off got no
	// summary and, with it, no title: every meeting in the window read as a
	// timestamp. The model being ready is the only real requirement.
	if !cfg.SummaryEnabled || !a.llmReady(cfg) {
		return
	}
	if err := a.limitedLLM(cfg); err != nil {
		log.Printf("meeting summarize: %v", err)
		return
	}
	defer trackLLM("Meeting, "+start.Format("15:04"), "Summarising")()

	entities := a.entityNames(cfg)
	modelDir := asr.ModelDir(a.modelsDir, llm.Spec)
	title, summary, entity, err := a.llm.Summarize(modelDir, text, entities, llm.SummaryOptions{
		Length: cfg.SummaryLength,
		Extra:  cfg.SummaryPromptExtra,
	}, a.llmEndpoint(cfg))
	a.noteLLMResult(cfg, err)
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
		notifyPane("No model selected. Pick one in Settings.", ui.PaneSettings)
		a.openSettingsForMissingModel()
		return spec, false
	}
	if !asr.IsDownloaded(a.modelsDir, spec) {
		log.Printf("model %s/%s not downloaded", spec.Family, spec.Variant)
		notifyPane("Model not downloaded yet. Download it in Settings first.", ui.PaneSettings)
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
	if cfg.LLMProvider == settings.LLMProviderOpenRouter {
		if len(cfg.OpenRouterAccounts) == 0 || cfg.OpenRouterActive == "" {
			return errors.New("add an OpenRouter account first")
		}
		if strings.TrimSpace(cfg.OpenRouterModel) == "" {
			return errors.New("choose a model first")
		}
		if err := llm.CheckOpenRouterKey(ep.APIKey); err != nil {
			return err
		}
	}
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
	err := llm.Ping(ep)
	a.noteLLMResult(cfg, err)
	return err
}

type cachedQuota struct {
	q  llm.FreeQuota
	at time.Time
}

// quotaTTL is how long a read count is shown before it is read again.
const quotaTTL = 20 * time.Second

// openRouterQuotas is how today's free requests stand for each account with
// a key, for the panes. The count comes from OpenRouter itself; an account
// found used up is marked limited (see limitedLLM) before any request is
// refused, and one found with requests left again is cleared.
func (a *app) openRouterQuotas(ids []string) map[string]ui.OpenRouterQuota {
	out := map[string]ui.OpenRouterQuota{}
	for _, id := range ids {
		key, err := a.secrets.Get(secrets.OpenRouterService, id)
		if err != nil || llm.CheckOpenRouterKey(key) != nil {
			continue
		}
		q, ok := a.freeQuota(id, key)
		entry := ui.OpenRouterQuota{Used: q.Used, Limit: q.Limit, Remaining: q.Remaining}
		if ok {
			a.orLimitMu.Lock()
			if q.Remaining > 0 {
				delete(a.orLimits, id)
			} else if _, known := a.orLimits[id]; !known {
				if a.orLimits == nil {
					a.orLimits = map[string]time.Time{}
				}
				a.orLimits[id] = nextUTCMidnight()
			}
			a.orLimitMu.Unlock()
		}
		if reset := a.openRouterLimit(id); !reset.IsZero() {
			entry.Reset = reset.UnixMilli()
			entry.Remaining = 0
		}
		out[id] = entry
	}
	return out
}

// freeQuota is the cached count for id, read again once it is older than
// quotaTTL. ok is false when it could not be read at all.
func (a *app) freeQuota(id, key string) (llm.FreeQuota, bool) {
	a.orLimitMu.Lock()
	c, hit := a.orQuotas[id]
	a.orLimitMu.Unlock()
	if hit && time.Since(c.at) < quotaTTL {
		return c.q, true
	}
	q, err := llm.KeyFreeQuota(llm.OpenRouterBaseURL, key)
	if err != nil {
		log.Printf("openrouter: reading the free request count: %v", err)
		return c.q, hit
	}
	a.orLimitMu.Lock()
	if a.orQuotas == nil {
		a.orQuotas = map[string]cachedQuota{}
	}
	a.orQuotas[id] = cachedQuota{q: q, at: time.Now()}
	a.orLimitMu.Unlock()
	return q, true
}

// nextUTCMidnight is when OpenRouter's free allowance comes back.
func nextUTCMidnight() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
}

// openRouterLimit is when account id's free daily limit comes back, or zero
// if it has not run out (or has already come back).
func (a *app) openRouterLimit(id string) time.Time {
	a.orLimitMu.Lock()
	defer a.orLimitMu.Unlock()
	reset, ok := a.orLimits[id]
	if !ok || time.Now().After(reset) {
		return time.Time{}
	}
	return reset
}

// limitedLLM is the error for a request not worth sending: the active
// OpenRouter account has used up its free requests for today, and asking
// again before the reset only burns time -- which, before a paste, the user
// is waiting through.
func (a *app) limitedLLM(cfg settings.Settings) error {
	if cfg.LLMProvider != settings.LLMProviderOpenRouter {
		return nil
	}
	if reset := a.openRouterLimit(cfg.OpenRouterActive); !reset.IsZero() {
		return fmt.Errorf("the OpenRouter account's free daily limit is used up until %s", reset.Local().Format("15:04"))
	}
	return nil
}

// noteLLMResult keeps track of the active OpenRouter account's daily limit:
// a refusal marks the account until the reset (with one notification, so
// the missing tasks and summaries have a reason), and any success clears it
// -- credits added, or the reset has passed early.
func (a *app) noteLLMResult(cfg settings.Settings, err error) {
	if cfg.LLMProvider != settings.LLMProviderOpenRouter || cfg.OpenRouterActive == "" {
		return
	}
	id := cfg.OpenRouterActive
	// Whatever the outcome, a request may have moved the count.
	a.orLimitMu.Lock()
	delete(a.orQuotas, id)
	a.orLimitMu.Unlock()
	var limit *llm.RateLimitError
	if err == nil || !errors.As(err, &limit) || !limit.Daily {
		if err == nil {
			a.orLimitMu.Lock()
			_, was := a.orLimits[id]
			delete(a.orLimits, id)
			a.orLimitMu.Unlock()
			if was {
				ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
			}
		}
		return
	}
	reset := limit.Reset
	if reset.IsZero() {
		// Not said: OpenRouter's day is UTC's.
		reset = nextUTCMidnight()
	}
	a.orLimitMu.Lock()
	if a.orLimits == nil {
		a.orLimits = map[string]time.Time{}
	}
	_, known := a.orLimits[id]
	a.orLimits[id] = reset
	a.orLimitMu.Unlock()
	if !known {
		// Overview's AI line reads the limit when it is drawn.
		ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
		notifyPane("OpenRouter: the free limit for today is used up, until "+reset.Local().Format("15:04")+
			". Tasks and summaries wait until then, or switch to another account in Settings.", ui.PaneSettings)
	}
}
