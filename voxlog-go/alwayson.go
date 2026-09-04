package main

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/diarize"
	"voxlog-go/internal/history"
	"voxlog-go/internal/ui"
	"voxlog-go/internal/vad"
)

// Always-on listening: Voxlog watches the microphone and starts recording by
// itself when somebody is actually talking.
//
// The user asked for a gate, not a dictaphone. Recording the whole day and
// sorting it out afterwards would cost gigabytes of keyboard noise, hours of
// decode time and a permanent recording light, so nothing reaches a file
// until two checks agree that a stretch of audio is speech:
//
//  1. silero-vad (internal/vad) says the stretch is speech at all, and lasted
//     long enough to be a sentence rather than a cough.
//  2. a second look at the same samples: loud enough to be somebody in the
//     room, and -- when the voice model is on disk -- a voice embedding the
//     extractor is willing to produce at all. Music and a fan clear the first
//     check surprisingly often; they do not clear this one.
//
// Once both agree, the ordinary meeting recorder takes over: same files, same
// history, same transcription queue. The only differences are that nobody
// pressed a key (history.Meeting.AutoStarted) and that the recording ends by
// itself once the room has been quiet for long enough.

const (
	// prerollSeconds is how much audio the listener keeps behind it. A
	// recording that begins the moment speech is confirmed begins a second
	// or two into the first sentence, and the first sentence is usually the
	// one that says what the conversation is about.
	prerollSeconds = 12
	// listenPollInterval is how often the supervisor re-reads the settings,
	// checks the frontmost app, and asks a running auto-recording whether the
	// room has gone quiet. Nothing here is urgent to the second.
	listenPollInterval = 2 * time.Second
	// minAutoRecordingSeconds is the shortest auto-started recording worth
	// keeping. Below this it is a passing remark that already ended before
	// the recorder was even open.
	minAutoRecordingSeconds = 8
	// autoSpeechLevel is the second stage's loudness floor, on the same
	// scale as audio.Level. Well above the threshold that counts as "voiced"
	// for a call already in progress: this one decides whether to start
	// recording at all, and a false start is worse than a missed one.
	autoSpeechLevel = 0.08
	// autoSweepInterval is how often the retention pass over auto-started
	// recordings runs. Hourly: the window it enforces is measured in hours,
	// and the pass reads every meeting row.
	autoSweepInterval = time.Hour
)

// alwaysOn is the listening half of the app. It owns the microphone whenever
// nothing else does, and gives it up for as long as a recording runs.
type alwaysOn struct {
	mu sync.Mutex
	// paused is the user's own off switch, from the menu. Separate from the
	// setting: pausing is "not right now", not "stop offering this".
	paused bool
	// listening is whether the microphone is currently open for the gate.
	listening bool
	// autoRunning marks a recording this listener started, so it only ever
	// stops its own -- a meeting the user started by hand is theirs to end.
	autoRunning bool

	recorder *audio.Recorder
	gate     *vad.Gate
	// preroll is the rolling tail of what has been heard, handed to the
	// recorder when one starts. Guarded by mu; written from the audio
	// callback, read by the goroutine that starts the recording.
	preroll []float32
	// fetching guards the one-at-a-time download of the VAD model.
	fetching bool
	// sweptAt is when retention last ran over auto-started recordings.
	sweptAt time.Time
	// pending is set by the audio callback when the gate confirms speech,
	// and consumed by the supervisor. The callback must not start a
	// recording itself: it runs on the audio thread, and opening files and
	// devices there is exactly what stutters a capture.
	pending bool
}

// startAlwaysOn runs the listening supervisor for the life of the app. It is
// always running; whether it actually opens the microphone depends on the
// setting, the pause switch, and what else is recording.
func (a *app) startAlwaysOn() {
	go func() {
		for {
			a.tickAlwaysOn()
			time.Sleep(listenPollInterval)
		}
	}()
}

// tickAlwaysOn is one pass of the supervisor: decide whether the gate should
// be listening right now, act on anything it heard, and end an auto-started
// recording that has gone quiet.
func (a *app) tickAlwaysOn() {
	defer func() {
		// The supervisor is the one goroutine in the app that must never die:
		// it is what would otherwise leave the microphone open forever.
		if r := recover(); r != nil {
			log.Printf("PANIC in always-on listening: %v", r)
			a.stopListening()
		}
	}()

	cfg := a.store.Get()
	if !cfg.AlwaysOn || a.listen.isPaused() {
		a.stopListening()
		return
	}

	// Retention runs from here rather than on its own timer: this is the one
	// goroutine that already wakes up regularly and knows always-on is on.
	if time.Since(a.listen.lastSweep()) > autoSweepInterval {
		a.listen.noteSweep()
		go a.sweepAutoRecordings()
	}

	// The gate needs its own small model. Without it there is nothing to
	// gate with, and recording everything is the design this is not -- so
	// fetch it once, in the background, and listen from the next tick.
	if !asr.IsDownloaded(a.modelsDir, vad.Spec) {
		a.stopListening()
		a.fetchVADModel()
		return
	}

	a.stopAutoRecordingIfQuiet(cfg.AlwaysOnSplitMinutes)

	// While anything is recording -- an auto recording, a meeting the user
	// started, a dictation -- the microphone belongs to that, and the gate
	// has nothing to listen to and nothing to decide.
	if a.recordingInProgress() {
		a.stopListening()
		return
	}

	if excluded(ui.FrontmostAppName(), cfg.AlwaysOnExcludedApps) {
		a.stopListening()
		return
	}

	a.startListening()

	if a.listen.takePending() {
		a.beginAutoRecording()
	}
}

// excluded reports whether the frontmost app is on the user's list. Matched
// case-insensitively on the app's own name, which is what the list is
// written in.
func excluded(app string, list []string) bool {
	if app == "" {
		return false
	}
	for _, name := range list {
		if strings.EqualFold(strings.TrimSpace(name), app) {
			return true
		}
	}
	return false
}

// startListening opens the microphone for the gate, if it is not already
// open. Idempotent: the supervisor calls it on every tick.
func (a *app) startListening() {
	a.listen.mu.Lock()
	if a.listen.listening {
		a.listen.mu.Unlock()
		return
	}
	a.listen.mu.Unlock()

	gate, err := vad.New(asr.ModelDir(a.modelsDir, vad.Spec), vad.DefaultConfig())
	if err != nil {
		log.Printf("always-on: %v", err)
		return
	}

	cfg := a.store.Get()
	rec, err := audio.NewRecorder(cfg.InputDevice, cfg.MicGain)
	if err != nil {
		log.Printf("always-on: microphone: %v", err)
		gate.Close()
		return
	}

	// StartUnbuffered: nothing here is kept except the rolling pre-roll. The
	// whole point of the gate is that most of what it hears is thrown away.
	if err := rec.StartUnbuffered(func(chunk []float32) {
		a.listen.onChunk(chunk, a.confirmSpeech)
	}); err != nil {
		log.Printf("always-on: microphone: %v", err)
		rec.Close()
		gate.Close()
		return
	}

	a.listen.mu.Lock()
	a.listen.recorder, a.listen.gate, a.listen.listening = rec, gate, true
	a.listen.mu.Unlock()
	a.tray.setListening(true)
	log.Print("always-on: listening")
}

// stopListening closes the microphone and forgets what the gate heard.
// Idempotent, and safe to call from the supervisor on every tick.
func (a *app) stopListening() {
	a.listen.mu.Lock()
	rec, gate := a.listen.recorder, a.listen.gate
	wasListening := a.listen.listening
	a.listen.recorder, a.listen.gate, a.listen.listening = nil, nil, false
	a.listen.preroll = nil
	a.listen.pending = false
	a.listen.mu.Unlock()

	if rec != nil {
		rec.Stop()
		rec.Close()
	}
	if gate != nil {
		gate.Close()
	}
	if wasListening {
		a.tray.setListening(false)
		log.Print("always-on: stopped listening")
	}
}

// onChunk runs on the audio thread. It keeps the pre-roll rolling, pushes
// the chunk through the gate, and raises the pending flag when a stretch of
// speech clears both checks. It never touches files or devices: the
// supervisor does that, one tick later.
func (l *alwaysOn) onChunk(chunk []float32, confirm func(vad.Segment) bool) {
	l.mu.Lock()
	gate := l.gate
	if gate == nil {
		l.mu.Unlock()
		return
	}

	l.preroll = append(l.preroll, chunk...)
	if max := prerollSeconds * audio.SampleRate; len(l.preroll) > max {
		// Copy rather than reslice: reslicing keeps the whole backing array
		// alive, which is the leak this loop would otherwise run all day.
		trimmed := make([]float32, max)
		copy(trimmed, l.preroll[len(l.preroll)-max:])
		l.preroll = trimmed
	}
	segments := gate.Feed(chunk)
	l.mu.Unlock()

	for _, seg := range segments {
		if confirm(seg) {
			l.mu.Lock()
			l.pending = true
			l.mu.Unlock()
			return
		}
	}
}

// confirmSpeech is the second stage. The VAD has already said this stretch
// is speech; this asks whether it is somebody in the room.
func (a *app) confirmSpeech(seg vad.Segment) bool {
	if seg.Seconds() < minVoicedSeconds {
		return false
	}
	if audio.Level(seg.Samples) < autoSpeechLevel {
		return false
	}

	// The voice extractor is the strongest filter available, and it is
	// already on disk whenever diarization is set up. Where it is not, the
	// two checks above stand on their own rather than blocking the feature
	// on a download the user never asked for.
	if !asr.IsDownloaded(a.modelsDir, diarize.Spec) {
		return true
	}
	e := a.embedders.get(a.modelsDir)
	if e == nil {
		return true
	}
	return len(e.Compute(seg.Samples)) > 0
}

// beginAutoRecording hands the microphone from the gate to the recorder,
// carrying the pre-roll across so the recording starts on the first word
// rather than after it.
func (a *app) beginAutoRecording() {
	a.listen.mu.Lock()
	preroll := a.listen.preroll
	a.listen.preroll = nil
	a.listen.mu.Unlock()

	// The device has one capture client here, not two: the gate's recorder
	// is closed before the meeting's is opened, and the pre-roll is what
	// covers the gap.
	a.stopListening()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.meeting != nil || a.dictation != nil {
		return
	}
	a.startMeetingWith(preroll, true)
	if a.meeting == nil {
		return
	}
	a.listen.mu.Lock()
	a.listen.autoRunning = true
	a.listen.mu.Unlock()
	notifyPane("Recording — Voxlog heard a conversation.", "meetings")
}

// stopAutoRecordingIfQuiet ends an auto-started recording once the room has
// been quiet for splitMinutes. This is what turns a day of listening into
// separate meetings instead of one file that never ends.
//
// Only recordings this listener started: a meeting the user began by hand
// ends when they say it does, however long the silence.
func (a *app) stopAutoRecordingIfQuiet(splitMinutes float64) {
	a.listen.mu.Lock()
	auto := a.listen.autoRunning
	a.listen.mu.Unlock()
	if !auto {
		return
	}
	if splitMinutes <= 0 {
		splitMinutes = 5
	}

	a.mu.Lock()
	m := a.meeting
	if m == nil {
		// The user stopped it themselves, or it never started.
		a.mu.Unlock()
		a.listen.mu.Lock()
		a.listen.autoRunning = false
		a.listen.mu.Unlock()
		return
	}
	m.mu.Lock()
	quietFor := time.Since(m.lastVoiced)
	m.mu.Unlock()

	if quietFor < time.Duration(splitMinutes*float64(time.Minute)) {
		a.mu.Unlock()
		return
	}
	log.Printf("always-on: %v of quiet, ending the recording", quietFor.Round(time.Second))
	a.stopMeeting()
	a.mu.Unlock()

	a.listen.mu.Lock()
	a.listen.autoRunning = false
	a.listen.mu.Unlock()
}

// fetchVADModel downloads silero-vad once, in the background. Same shape as
// the diarization models' own fetch: one attempt at a time, and the caller
// simply tries again on its next tick.
func (a *app) fetchVADModel() {
	a.listen.mu.Lock()
	if a.listen.fetching {
		a.listen.mu.Unlock()
		return
	}
	a.listen.fetching = true
	a.listen.mu.Unlock()

	go func() {
		log.Print("always-on: downloading the voice-activity model")
		err := asr.Download(a.modelsDir, vad.Spec, func(string, int64, int64) {}, fetchURL)
		a.listen.mu.Lock()
		a.listen.fetching = false
		a.listen.mu.Unlock()
		if err != nil {
			log.Printf("always-on: download failed: %v", err)
			notify("Could not download the voice-activity model, so always-on listening is off.")
			return
		}
		log.Print("always-on: voice-activity model ready")
	}()
}

// Pause and its state are read by the menu item and by the supervisor.
func (l *alwaysOn) isPaused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paused
}

func (l *alwaysOn) setPaused(v bool) {
	l.mu.Lock()
	l.paused = v
	l.mu.Unlock()
}

func (l *alwaysOn) lastSweep() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweptAt
}

func (l *alwaysOn) noteSweep() {
	l.mu.Lock()
	l.sweptAt = time.Now()
	l.mu.Unlock()
}

// takePending reads and clears the flag the audio thread raises.
func (l *alwaysOn) takePending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.pending {
		return false
	}
	l.pending = false
	return true
}

// sweepAutoRecordings deletes auto-started recordings that never produced a
// transcript and are older than the retention window.
//
// Retention stops being optional once the app listens all day. A recording
// the gate opened and the transcript never justified is a guess that did not
// pay off: the audio goes, the history row stays (it is one line, and it is
// the only evidence the machine was listening at that hour). Anything with a
// transcript, and anything the user started by hand, is untouched.
func (a *app) sweepAutoRecordings() {
	cfg := a.store.Get()
	if !cfg.AlwaysOn || cfg.AlwaysOnRetentionHours <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(cfg.AlwaysOnRetentionHours * float64(time.Hour)))

	all, err := a.meetings.All()
	if err != nil {
		log.Printf("always-on sweep: %v", err)
		return
	}
	inUse := a.recordingsInUse()

	for _, m := range all {
		if !m.AutoStarted || m.Text != "" || m.Start.After(cutoff) {
			continue
		}
		if m.AudioPath == "" && m.SystemAudioPath == "" {
			continue
		}
		if inUse[m.AudioPath] || inUse[m.SystemAudioPath] {
			continue
		}
		for _, path := range []string{m.AudioPath, m.SystemAudioPath} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Printf("always-on sweep: removing %s: %v", path, err)
			}
		}
		if err := a.meetings.Update(m.Start, func(e *history.Meeting) {
			e.AudioPath, e.SystemAudioPath = "", ""
		}); err != nil {
			log.Printf("always-on sweep: clearing the audio path: %v", err)
		}
		log.Printf("always-on sweep: dropped the audio of %s, never transcribed",
			m.Start.Format(time.RFC3339))
	}
}

// toggleListenPause is what the menu item does: "not right now", without
// turning the feature off. Reports the state it landed in.
func (a *app) toggleListenPause() bool {
	paused := !a.listen.isPaused()
	a.listen.setPaused(paused)
	if paused {
		a.stopListening()
	}
	return paused
}

// listenPauseLabel is the menu item's two faces: what the click will do, not
// what the state is.
func listenPauseLabel(paused bool) string {
	if paused {
		return "Resume listening"
	}
	return "Pause listening"
}
