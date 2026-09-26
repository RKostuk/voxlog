package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/history"
	"voxlog-go/internal/hotkey"
	"voxlog-go/internal/llm"
	"voxlog-go/internal/output"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/ui"
)

// minHoldDuration is how long the dictate key has to be held, in hold mode,
// before the take counts. Below it the press was an accident -- a key brushed
// on the way to something else -- and decoding a tenth of a second of nothing
// would put an empty line in history and paste it into whatever was focused.
const minHoldDuration = 250 * time.Millisecond

// isAccidentalTap reports whether a held take was too short to be speech.
func isAccidentalTap(held time.Duration) bool { return held < minHoldDuration }

// dictation is one take in progress.
// dictationElapsed is how long the take in progress has been running, and
// false when there is none -- the menu's status line, the same shape as
// meetingElapsed.
func (a *app) dictationElapsed() (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dictation == nil {
		return false, 0
	}
	return true, time.Since(a.dictation.start)
}

type dictation struct {
	// The microphone is either this take's own...
	recorder *audio.Recorder
	// ...or a meeting's, which this take is listening in on. Opening the
	// device twice would work on most Macs and fail on some; listening in
	// cannot drift from what the meeting recorded, because it is the same
	// audio.
	borrowed *audio.Recorder
	detach   func()
	buf      []float32 // what was heard while borrowing
	bufMu    sync.Mutex
	// detachMeter drops the listener that drives the overlay's level meter
	// off a borrowed capture.
	detachMeter func()

	spec     asr.ModelSpec
	language string
	start    time.Time

	// live is non-nil only for a streaming take: chunks go to it through
	// liveChunks, and liveDone closes once its feeding goroutine has drained.
	live       asr.StreamingTranscriber
	liveChunks chan []float32
	liveDone   chan struct{}
}

// level is the loudest moment since the last read, for the overlay's meter.
func (d *dictation) level() float64 {
	if d.recorder != nil {
		return d.recorder.Level()
	}
	// A meeting has no meter of its own, so reading the shared recorder's
	// peak here takes nothing away from anyone.
	return d.borrowed.Level()
}

// take ends the capture and returns everything heard.
func (d *dictation) take() []float32 {
	if d.recorder != nil {
		samples := d.recorder.Stop()
		d.recorder.Close()
		return samples
	}
	d.detach()
	d.bufMu.Lock()
	defer d.bufMu.Unlock()
	return d.buf
}

// startDictation opens a capture and puts the overlay up. a.mu must be held:
// everything here is a few milliseconds of setup, and holding the lock across
// it is what makes a second hotkey press wait rather than race.
func (a *app) startDictation() {
	a.stopBackfillLocked() // same reason as in startMeeting

	cfg := a.store.Get()
	spec, ok := a.model(cfg)
	if !ok {
		return
	}

	d := &dictation{spec: spec, language: cfg.Language}

	// A meeting owns the microphone while it runs; this take listens in on it
	// rather than opening a second capture (see the dictation struct).
	if a.meeting != nil {
		d.borrowed = a.meeting.recorder
		d.detach = d.borrowed.Attach(func(chunk []float32) {
			d.bufMu.Lock()
			d.buf = append(d.buf, chunk...)
			d.bufMu.Unlock()
		})
	} else {
		rec, err := audio.NewRecorder(cfg.InputDevice, cfg.MicGain)
		if err != nil {
			log.Printf("recorder init: %v", err)
			notifyPane("Could not access the microphone.", ui.PaneHistory)
			return
		}
		d.recorder = rec
	}

	// Before the overlay is touched, so a freeze can be placed: every other
	// line the start of a take writes comes from inside a main-thread closure
	// (see Overlay.Show), which means a blocked main thread looks exactly like
	// a hotkey that never fired. This line tells those two apart.
	log.Printf("DEBUG dictation: starting, style=%s position=%s", cfg.IndicatorStyle, cfg.OverlayPosition)

	// Read the placement straight off the settings each take, rather than
	// pushing it into the overlay when Settings saves: this path already holds
	// fresh settings, so there is nothing to keep in sync and a change applies
	// to the very next dictation.
	a.overlay.Configure(cfg.IndicatorStyle, ui.IndicatorParts{
		Wave:       cfg.IndicatorWave,
		Timer:      cfg.IndicatorTimer,
		ModeLabel:  cfg.IndicatorModeLabel,
		StopButton: cfg.IndicatorStopButton,
		Solid:      cfg.IndicatorSolid,
		Outline:    cfg.IndicatorOutline,
		WaveWidth:  cfg.IndicatorWaveWidth,
		WaveHeight: cfg.IndicatorWaveHeight,
	}, cfg.OverlayPosition)
	a.overlay.SetShortcut(hotkey.ParseBinding(cfg.DictateKeyID).Label())
	// On screen before capture begins: started after, the opening levels are
	// pushed into a window that isn't visible yet, and the meter looks dead
	// for the first moment of speech.
	a.overlay.Show()

	// Live streaming needs the model resident BEFORE audio starts, since
	// chunks are decoded as they arrive. That means paying the load here
	// (usually already warm from the preload) instead of in the background as
	// the batch path does.
	if spec.SupportsStreaming && cfg.LiveStreamingText {
		if !a.startLiveDecoder(d, spec, cfg) {
			a.abandon(d)
			return
		}
	}

	if err := a.startCapture(d); err != nil {
		log.Printf("recorder start: %v", err)
		notifyPane("Could not start recording.", ui.PaneHistory)
		a.abandon(d)
		return
	}

	d.start = time.Now()
	a.dictation = d
	a.tray.startedRecording()

	// Capture is already running; load the model concurrently so it's ready by
	// the time the user stops talking. If it's still loading then, the stop
	// path's get() just waits on the same mutex.
	go a.models.warm(spec, asr.ModelDir(a.modelsDir, spec), cfg.Language)
}

// startLiveDecoder wires up the streaming recognizer for takes that show text
// while you speak. Reports whether the take can go ahead.
func (a *app) startLiveDecoder(d *dictation, spec asr.ModelSpec, cfg settings.Settings) bool {
	t, err := a.models.get(spec, asr.ModelDir(a.modelsDir, spec), cfg.Language)
	if err != nil {
		log.Printf("live streaming unavailable: %v", err)
		notifyPane("Could not load the speech model.", ui.PaneSettings)
		return false
	}
	st, ok := t.(asr.StreamingTranscriber)
	if !ok {
		return true // model does not stream after all; the batch path handles it
	}

	d.live = st
	// Deep enough that a decoder briefly behind realtime never forces the
	// callback to throw audio away: chunks are ~30ms, so this holds roughly
	// two minutes of speech for about 8MB. An earlier 64-slot buffer dropped
	// the opening words of anything the model couldn't keep up with, which
	// read as "it cut the beginning off".
	d.liveChunks = make(chan []float32, 4096)
	d.liveDone = make(chan struct{})
	go func(st asr.StreamingTranscriber, in <-chan []float32, done chan<- struct{}) {
		defer close(done)
		for chunk := range in {
			partial, err := st.Feed(chunk)
			if err != nil {
				log.Printf("live feed: %v", err)
				continue
			}
			a.overlay.SetText(partial)
		}
	}(d.live, d.liveChunks, d.liveDone)
	return true
}

// startCapture begins the microphone side of the take. A borrowed capture is
// already running, so there is only the meter to keep fed.
func (a *app) startCapture(d *dictation) error {
	var lastLevelPush time.Time
	// The clock rides along with the meter rather than owning a ticker: it
	// only ever changes once a second, and a goroutine per take to say so
	// would outnumber the work it does.
	lastElapsed := -1
	pushLevel := func() {
		// The audio callback runs on miniaudio's realtime thread -- keep it to
		// cheap work, and throttle the webview hop so a 16kHz stream can't
		// flood the main queue.
		if time.Since(lastLevelPush) < 40*time.Millisecond {
			return
		}
		lastLevelPush = time.Now()
		// The recorder's own level, not audio.Level(chunk): the chunk has
		// already had gain applied, and measuring that counts the gain twice,
		// pinning the equalizer at full.
		a.overlay.SetLevel(d.level())

		if secs := int(time.Since(d.start).Seconds()); secs != lastElapsed {
			lastElapsed = secs
			a.overlay.SetElapsed(float64(secs))
		}
	}

	if d.recorder == nil {
		// Borrowed: the meeting's capture is the source, and the listener that
		// buffers it is already attached. Meter off the same chunks.
		d.detachMeter = d.borrowed.Attach(func([]float32) { pushLevel() })
		return nil
	}

	chunks := d.liveChunks
	return d.recorder.StartStreaming(func(chunk []float32) {
		if chunks != nil {
			select {
			case chunks <- chunk:
			default:
				// Only reachable if the decoder falls minutes behind. Still
				// never block here -- this is miniaudio's realtime thread --
				// but say so rather than losing words silently.
				log.Printf("live streaming: decoder behind, dropped a chunk")
			}
		}
		pushLevel()
	})
}

// abandon tears down a take that never got started.
func (a *app) abandon(d *dictation) {
	if d.liveChunks != nil {
		close(d.liveChunks)
		<-d.liveDone
		d.live.Finish() // reset the stream so the next take starts clean
	}
	if d.recorder != nil {
		d.recorder.Close()
	} else if d.detach != nil {
		d.detach()
	}
	if d.detachMeter != nil {
		d.detachMeter()
	}
	a.overlay.Hide()
}

// stopDictation ends the take and queues it for transcription. Nothing here
// waits for a transcript: the samples go to the decode queue and the hotkey is
// free again immediately.
func (a *app) stopDictation() {
	d := a.dictation
	a.dictation = nil
	a.reclaimMic()
	cfg := a.store.Get()

	samples := d.take()
	if d.detachMeter != nil {
		d.detachMeter()
	}

	recordingSeconds := time.Since(d.start).Seconds()
	log.Printf("DEBUG mic: %.1fs, peak %.1f dBFS at gain %.2f (%.0f%%)",
		float64(len(samples))/audio.SampleRate, audio.PeakDBFS(samples), cfg.MicGain, cfg.MicGain*100)

	// A streaming take has already been decoded chunk by chunk, so its
	// transcript comes from flushing the stream rather than from the buffer.
	liveText := ""
	if d.live != nil {
		close(d.liveChunks)
		<-d.liveDone
		final, err := d.live.Finish()
		if err != nil {
			log.Printf("transcribe (streaming): %v", err)
			notifyPane("Transcription failed.", ui.PaneHistory)
			a.finishRecording(cfg, false)
			return
		}
		liveText = final
	}

	if len(samples) == 0 {
		a.finishRecording(cfg, false) // nothing captured (stopped before audio started)
		return
	}

	a.finishRecording(cfg, true)
	a.queue.submit("Dictation, "+d.start.Format("15:04"), "", recordingSeconds, func(yield func()) {
		start := time.Now()
		// A dictation is the microphone alone: what is playing on the Mac
		// belongs to a meeting, never to a note.
		text := liveText
		if text == "" {
			text = a.transcribeSamples(d.spec, d.language, samples, nil, false, yield)
		}

		a.releaseOverlay()
		a.recordDictationResult(cfg, text, start, d.start, recordingSeconds, samples)
	})
}

// recordDictationResult finishes a take: the text is emitted (unless empty --
// there is nothing to paste or copy) but the history entry, and the audio
// behind it, are always saved, even for a take that decoded to nothing. That
// take's audio is exactly what settings.KeepDictationAudio is for: the
// recording is the only thing a wrong -- or missing -- transcript can be
// checked against, so it must not be the one case where saving it is skipped.
//
// With output.ModePasteUnlessTask the LLM is asked first, here, before
// anything is pasted: a dictation it reads as a task becomes the task and is
// not pasted; anything else -- no task, or no answer (a rate limit, no key)
// -- is pasted as usual, so a failing LLM never swallows the text.
func (a *app) recordDictationResult(cfg settings.Settings, text string, start, dictStart time.Time, recordingSeconds float64, samples []float32) {
	var found []llm.Result
	asked := false
	if text != "" {
		mode := cfg.OutputMode
		if mode == output.ModePasteUnlessTask {
			asked = true
			results, err := a.findTasks(history.KindDictation, text)
			if err != nil {
				log.Printf("task classify: %v -- pasting the text instead", err)
			}
			found = results
			mode = dictationOutputMode(mode, found)
		}
		if err := output.Emit(text, mode); err != nil {
			log.Printf("output: %v", err)
		}
	}
	ts := time.Now()
	if err := a.hist.Append(history.Entry{
		Timestamp:        ts,
		DurationSeconds:  time.Since(start).Seconds(),
		Text:             text,
		RecordingSeconds: recordingSeconds,
		AudioPath:        a.saveDictationAudio(dictStart, samples),
	}); err != nil {
		log.Printf("history append: %v", err)
	} else {
		// The window doesn't poll -- without this, a dictation taken while
		// History is already open just sits missing from the list until the
		// user closes and reopens it (meeting.go's equivalent paths already
		// do this; this one never did).
		ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
		switch {
		case len(found) > 0:
			a.saveTasks(history.KindDictation, ts.Format(time.RFC3339Nano), found)
			notifyPane(taskSavedMessage(found), ui.PaneTasks)
		case text != "" && !asked:
			go a.classifyForTasks(history.KindDictation, ts.Format(time.RFC3339Nano), text)
		}
	}
	go a.sweepRecordings()
}

// saveDictationAudio writes a finished take beside the meeting recordings and
// returns where it landed, or "" if it could not be kept. A take whose audio
// fails to save is still a transcript worth having, so this never stops the
// entry being written.
func (a *app) saveDictationAudio(at time.Time, samples []float32) string {
	if !a.store.Get().KeepDictationAudio || len(samples) == 0 {
		return ""
	}

	dir := filepath.Join(a.hist.Dir(), meetingsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("dictation audio: %v", err)
		return ""
	}
	path := filepath.Join(dir, at.Format("2006-01-02-150405")+"-dictation.wav")

	w, err := audio.NewWAVWriter(path)
	if err != nil {
		log.Printf("dictation audio: %v", err)
		return ""
	}
	if err := w.Write(samples); err != nil {
		log.Printf("dictation audio: %v", err)
		w.Close()
		return ""
	}
	if err := w.Close(); err != nil {
		log.Printf("dictation audio: %v", err)
		return ""
	}
	return path
}

// finishRecording moves the indicators from "recording" to whatever comes
// next: the decode, if there is one to wait for, or nothing.
func (a *app) finishRecording(cfg settings.Settings, decoding bool) {
	a.tray.stoppedRecording()
	a.overlay.ClearText()
	switch {
	case !decoding:
		a.overlay.SetTranscribing(false)
		a.overlay.Hide()
	case cfg.ShowTranscribingOverlay:
		// Decoding can take a noticeable moment on the larger models. The menu
		// bar always says so -- it is a glyph nobody has to look at -- but the
		// capsule staying up in the middle of the screen is opt-in, since it
		// keeps drawing the eye to a window with nothing left to tell you.
		a.overlay.SetTranscribing(true)
	default:
		a.overlay.Hide()
	}
}

// toggleDictate is the tap-to-start, tap-to-stop behaviour of the dictate key.
func (a *app) toggleDictate() {
	a.mu.Lock()
	running := a.dictation != nil
	a.mu.Unlock()

	if running {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.dictation != nil {
			a.stopDictation()
		}
		return
	}

	// Always-on gives up the microphone first, and outside the lock: a
	// dictation is the one case where the user is talking TO the machine,
	// and the gate reacting to those same words is the one thing it must
	// never do.
	a.yieldMicToUser()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dictation != nil {
		// Raced with another press; the second one is the stop.
		a.stopDictation()
		return
	}
	a.startDictation()
}

// holdDictate starts a take when the dictate key goes down in hold mode.
func (a *app) holdDictate() {
	a.mu.Lock()
	running := a.dictation != nil
	a.mu.Unlock()
	if running {
		return
	}

	a.yieldMicToUser()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dictation == nil {
		a.startDictation()
	}
}

// releaseDictate ends a held take. A press too short to be speech is thrown
// away rather than decoded.
func (a *app) releaseDictate() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.dictation == nil {
		return // cancelled while held, or never started
	}
	if isAccidentalTap(time.Since(a.dictation.start)) {
		log.Printf("dictation: %v hold, discarded as an accidental tap", time.Since(a.dictation.start))
		a.cancelDictationLocked()
		return
	}
	a.stopDictation()
}

// cancelDictate throws away an in-progress recording: stop the mic, drop the
// samples, no transcription, no history entry. A no-op when nothing is
// recording, so a stray Escape press costs nothing. A running meeting is
// untouched -- only the dictation inside it is cancelled.
func (a *app) cancelDictate() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelDictationLocked()
}

func (a *app) cancelDictationLocked() {
	d := a.dictation
	if d == nil {
		return
	}
	a.dictation = nil
	a.reclaimMic()

	d.take() // discard whatever was captured
	if d.detachMeter != nil {
		d.detachMeter()
	}
	if d.liveChunks != nil {
		close(d.liveChunks)
		<-d.liveDone
		d.live.Finish() // reset the stream so the next take starts clean
	}
	a.tray.stoppedRecording()
	a.overlay.SetTranscribing(false)
	a.overlay.ClearText()
	a.overlay.Hide()
}

// taskSavedMessage says where a dictation went when it was not pasted:
// without it, a take that turned into a task looks like one that was lost.
func taskSavedMessage(found []llm.Result) string {
	if len(found) == 1 {
		return "Saved as a task: " + found[0].Text
	}
	return fmt.Sprintf("Saved as %d tasks", len(found))
}

// dictationOutputMode is how a take is delivered once the classifier has
// answered: a take that became tasks is not pasted; everything else goes out
// as mode says.
func dictationOutputMode(mode string, found []llm.Result) string {
	if mode == output.ModePasteUnlessTask && len(found) > 0 {
		return output.ModeNone
	}
	return mode
}
