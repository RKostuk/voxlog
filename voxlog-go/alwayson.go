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
	"voxlog-go/internal/settings"
	"voxlog-go/internal/systemaudio"
	"voxlog-go/internal/ui"
	"voxlog-go/internal/vad"
)

// Always-on listening.
//
// The microphone is open for as long as the mode is on, but nothing reaches
// the disk until somebody is actually talking. What is held instead is a
// rolling buffer in memory -- a minute of it, four megabytes -- so a
// recording that starts on the second word still contains the first.
//
// Two questions are answered live, not afterwards:
//
//   - Is this speech? silero-vad says where speech starts and ends
//     (internal/vad), and a second look at the same samples -- loud enough,
//     and a voice the embedding extractor will accept -- throws out the
//     typing, the fan and most of the music.
//
//   - Is this a note or a conversation? Every reply the gate emits is
//     fingerprinted and folded into the session's running speakers. A second
//     voice that says enough turns the recording into a meeting, on the spot,
//     in the same file. So a couple of remarks to yourself are two notes in
//     History, and the conversation that starts right after them is a meeting
//     -- without anything being decided in advance, and without an LLM.
//
// The verdict is what routes the recording when it ends: History for a note
// (never pasted anywhere -- nobody asked for it), Meetings for a
// conversation, with the diarization and turns that pane needs.

const (
	// prerollSeconds is how much audio is kept behind the gate. It only has
	// to cover the gate's own delay now that a recording opens on live
	// speech rather than on a finished sentence, so it is a fraction of what
	// it was -- and a fraction of the memory, since it rolls all day.
	prerollSeconds = 15
	// listenPollInterval is how often the supervisor re-reads the settings,
	// checks the frontmost app, and asks an open session whether the room
	// has gone quiet. Nothing here is urgent to the second.
	listenPollInterval = 2 * time.Second
	// workQueue is how much audio may wait for the worker, in callback
	// chunks. miniaudio delivers roughly every 10ms, so this is about eight
	// seconds -- enough to ride out a model call without the audio thread
	// ever having to throw a chunk away. It used to be 128, which is a
	// second and a quarter, and a single embedding over a long reply was
	// enough to overrun it.
	workQueue = 800
	// chunkSeconds is roughly how much audio one capture callback carries,
	// for turning a dropped-chunk count into something a person can read.
	chunkSeconds = 0.01
	// embedSeconds caps how much of a reply the voice extractor is given.
	// It is deciding whether two stretches are the same person, and three
	// seconds answers that as well as sixty do -- while sixty means the
	// worker is gone for long enough that the audio behind it is dropped and
	// the gate stops hearing the conversation it is supposed to be timing.
	embedSeconds = 3.0
	// autoSpeechLevel is the second stage's loudness floor, on the same scale
	// as audio.Level. Well above the "voiced" threshold used inside a
	// recording already in progress: this one decides whether to start
	// recording at all, and a false start is worse than a missed one.
	autoSpeechLevel = 0.08
	// minVoicedToKeep is how much of a recording has to be speech, by the
	// gate's own reckoning, before it is worth keeping at all. Below this
	// the gate's first impression was a door, a cough, or a phrase from a
	// video.
	minVoicedToKeep = 1.0
	// minAutoRecordingSeconds is the shortest session worth keeping. Below
	// this it is a passing noise that cleared the gate.
	minAutoRecordingSeconds = 3
	// provisionalGrace is how long a recording that nothing has confirmed
	// yet is allowed to run. A session opens the moment the gate hears a
	// voice, so the menu bar can say "recording" while the first sentence is
	// still being said; if the second stage never agrees, this is how long
	// it takes for that guess to be thrown away.
	provisionalGrace = 12 * time.Second
	// maxSessionHours caps a single recording, however lively the room. A
	// file that never ends is one nobody can play, transcribe or delete.
	maxSessionHours = 2
	// autoSweepInterval is how often retention runs over auto recordings.
	// Hourly: the window it enforces is measured in hours.
	autoSweepInterval = time.Hour
)

// listenMsg is what the capture callbacks hand the worker. Everything the
// gates ever see arrives this way, so the gates need no lock of their own:
// one goroutine touches them, from creation to close.
type listenMsg struct {
	kind    int
	samples []float32
	gate    *vad.Gate
}

const (
	// msgMic and msgFar are audio from the two capture paths.
	msgMic = iota
	msgFar
	// msgFarGateOpen and msgFarGateClose hand the far-end gate to the worker
	// and take it back. The tap starts and stops on the supervisor's
	// schedule, not the worker's, and routing the handover through the same
	// channel is what keeps the gate single-threaded anyway.
	msgFarGateOpen
	msgFarGateClose
)

// alwaysOn is the listening half of the app: it owns the microphone, the
// gate, and whatever session is currently open.
type alwaysOn struct {
	mu sync.Mutex
	// paused is the user's own off switch, from the menu. Separate from the
	// setting: pausing is "not right now", not "stop offering this".
	paused bool
	// listening is whether the microphone is currently open for the gate.
	listening bool

	recorder *audio.Recorder
	// work carries audio from the capture callback to the worker goroutine,
	// which owns both voice-activity gates and does every model call. The
	// callback must stay cheap: silero on every window and CAM++ on every
	// reply used to run on the capture thread, and that is what made the app
	// stutter. nil while not listening, which is also how the callback knows
	// there is nobody to send to.
	work chan listenMsg
	// workerDone closes when the worker has drained work and released its
	// gates, so stopListening can wait rather than race it.
	workerDone chan struct{}
	// dropped counts chunks the audio thread had to throw away because the
	// worker was still busy. Audio must never block on the worker, so a full
	// channel drops -- but this used to be a bool that logged one line per
	// run, which hid the fact that dropping is what makes the gate lose the
	// conversation it is timing. Reported when the session it affected ends.
	dropped int
	// tapRunning is whether this listener started the system-audio tap.
	tapRunning bool

	// preroll and farPreroll are the rolling tails of what has been heard.
	// Written from the audio callbacks, drained when a session opens.
	preroll    []float32
	farPreroll []float32

	// sess is the recording in progress, or nil. Owned here rather than on
	// app.meeting: a session may still turn out to be a note, and it is
	// always-on that feeds it audio.
	sess *session
	// sessKind is what the session was last seen to be, so the supervisor
	// can pick the right silence threshold without taking the session's lock
	// on every tick.
	sessKind string

	// micMuted is "do not write me down", and unlike the meeting's mute it
	// is a state of the mode, not of one recording: switch it on and it
	// stays on across sessions until switched off. Always-on listens all
	// day, so "not right now" has to be one click and has to survive the
	// session it was clicked in.
	//
	// It silences one side only. The far end of a call keeps recording,
	// which is the difference from pausing: a call you are sitting quietly
	// through is exactly what the tap is for.
	micMuted bool

	// yielded is set while a dictation owns the microphone. It exists so the
	// worker never has to ask the app whether the user is recording:
	// answering that takes a.mu, and a.mu is held by the very hotkey path
	// that waits for the worker to drain -- which deadlocks. This flag lives
	// under listen.mu with everything else the worker touches.
	yielded bool
	// wake nudges the supervisor between its own ticks. Buffered by one and
	// sent to without blocking: it is a "look again now", and two of them
	// pending mean the same thing as one.
	wake chan struct{}
	// fetching guards the one-at-a-time download of the VAD model.
	fetching bool
	// sweptAt is when retention last ran over auto recordings.
	sweptAt time.Time
}

// startAlwaysOn runs the listening supervisor for the life of the app. It is
// always running; whether it opens the microphone depends on the setting,
// the pause switch, the frontmost app, and what else is recording.
func (a *app) startAlwaysOn() {
	a.listen.mu.Lock()
	a.listen.wake = make(chan struct{}, 1)
	wake := a.listen.wake
	a.listen.mu.Unlock()

	go func() {
		for {
			a.tickAlwaysOn()
			// A dictation handing the microphone back is worth acting on
			// now. Waiting for the next tick meant the room was not being
			// listened to for as long as two seconds after the user
			// finished dictating -- which is exactly when they carry on
			// talking.
			select {
			case <-wake:
			case <-time.After(listenPollInterval):
			}
		}
	}()
}

// nudge asks the supervisor to take another look immediately.
func (l *alwaysOn) nudge() {
	l.mu.Lock()
	wake := l.wake
	l.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// tickAlwaysOn is one pass of the supervisor.
func (a *app) tickAlwaysOn() {
	defer func() {
		// The supervisor is the one goroutine that must never die: it is what
		// would otherwise leave the microphone open forever.
		if r := recover(); r != nil {
			log.Printf("PANIC in always-on listening: %v", r)
			a.closeSession()
			a.stopListening()
		}
	}()

	cfg := a.store.Get()
	if !cfg.AlwaysOn || a.listen.isPaused() {
		a.closeSession()
		a.stopListening()
		return
	}

	// Retention runs from here rather than on its own timer: this is the one
	// goroutine that already wakes regularly and knows always-on is on.
	if time.Since(a.listen.lastSweep()) > autoSweepInterval {
		a.listen.noteSweep()
		go a.sweepAutoRecordings()
	}

	// The gate needs its own small model. Without it there is nothing to
	// gate with, and recording everything is the design this is not -- so
	// fetch it once, in the background, and listen from the next tick.
	if !asr.IsDownloaded(a.modelsDir, vad.Spec) {
		a.closeSession()
		a.stopListening()
		a.fetchVADModel()
		return
	}

	// A dictation or a meeting the user started by hand owns the microphone
	// and the subject; always-on steps aside entirely.
	if a.listen.isYielded() || a.userRecordingInProgress() {
		a.closeSession()
		a.stopListening()
		return
	}

	if excluded(ui.FrontmostAppName(), cfg.AlwaysOnExcludedApps) {
		a.closeSession()
		a.stopListening()
		return
	}

	a.startListening(cfg)
	a.syncTap(cfg)
	// Recordings are opened by the worker, the instant it hears a voice --
	// waiting for this tick put up to two seconds between speaking and the
	// menu bar saying so. The supervisor only ever ends them.
	a.closeSessionIfDone(cfg)
}

// userRecordingInProgress is a recording the user started themselves, as
// opposed to the session always-on may have open.
func (a *app) userRecordingInProgress() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meeting != nil || a.dictation != nil
}

// excluded reports whether the frontmost app is on the user's list. Matched
// case-insensitively on the app's own name, which is what the list holds.
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

// startListening opens the microphone for the gate. Idempotent: the
// supervisor calls it on every tick.
//
// The microphone is opened once and stays open across a session: the session
// is written from this same stream. Stopping one recorder to start another
// would drop the moment the recording is about to be about.
func (a *app) startListening(cfg settings.Settings) {
	a.listen.mu.Lock()
	already := a.listen.listening
	a.listen.mu.Unlock()
	if already {
		return
	}

	// Timed, because how long this takes is the difference between
	// always-on being back the moment a dictation ends and it being deaf
	// for another ten seconds. Loading the gate's model and opening the
	// device are the two candidates, and only a live run can say which.
	started := time.Now()
	defer func() {
		if slow := time.Since(started); slow > time.Second {
			log.Printf("DEBUG always-on: took %v to start listening", slow.Round(time.Millisecond))
		}
	}()

	gate, err := vad.New(asr.ModelDir(a.modelsDir, vad.Spec), vad.DefaultConfig())
	if err != nil {
		log.Printf("always-on: %v", err)
		return
	}
	rec, err := audio.NewRecorder(cfg.InputDevice, cfg.MicGain)
	if err != nil {
		log.Printf("always-on: microphone: %v", err)
		gate.Close()
		return
	}

	// StartUnbuffered: nothing is kept except the rolling pre-roll and
	// whatever a session is writing. Most of what this hears is thrown away,
	// which is the point.
	work := make(chan listenMsg, workQueue)
	done := make(chan struct{})

	a.listen.mu.Lock()
	a.listen.recorder, a.listen.work, a.listen.workerDone = rec, work, done
	a.listen.listening = true
	a.listen.mu.Unlock()

	go a.listenWorker(gate, work, done)

	if err := rec.StartUnbuffered(a.onListenChunk); err != nil {
		log.Printf("always-on: microphone: %v", err)
		a.listen.mu.Lock()
		a.listen.recorder, a.listen.work, a.listen.workerDone = nil, nil, nil
		a.listen.listening = false
		a.listen.mu.Unlock()
		close(work)
		<-done
		rec.Close()
		return
	}

	a.tray.setListening(true)
	log.Print("always-on: listening")
}

// listenWorker is the only goroutine that touches a gate or a model. It ends
// when work is closed, releasing both gates on the way out.
func (a *app) listenWorker(gate *vad.Gate, work <-chan listenMsg, done chan<- struct{}) {
	defer close(done)
	defer gate.Close()

	var farGate *vad.Gate
	defer func() {
		if farGate != nil {
			farGate.Close()
		}
	}()

	for msg := range work {
		switch msg.kind {
		case msgFarGateOpen:
			if farGate != nil {
				farGate.Close()
			}
			farGate = msg.gate
		case msgFarGateClose:
			if farGate != nil {
				farGate.Close()
				farGate = nil
			}
		case msgMic:
			segments := gate.Feed(msg.samples)
			// Opening on live speech, not on a finished sentence: silero
			// only emits a segment once the pause after it is long enough,
			// so waiting for one meant nothing on screen until the speaker
			// stopped talking -- twenty seconds of "is this thing on?" for
			// twenty seconds of speech.
			if gate.Speaking() {
				// Both jobs, and the second one is the one that was
				// missing: the session that opens here is also the session
				// whose silence timer has to know the room is still busy.
				// Leaving that to the confirmed replies alone meant a quiet
				// speaker had their recording closed underneath them and
				// the rest of what they said filed as a separate note.
				if sess := a.openSessionIfIdle(false); sess != nil {
					sess.heardVoice()
				}
			}
			a.handleMicSegments(segments)
		case msgFar:
			if farGate != nil {
				segments := farGate.Feed(msg.samples)
				if farGate.Speaking() {
					// The other side of a call is a second party by
					// definition, so this opens straight as a conversation.
					if sess := a.openSessionIfIdle(true); sess != nil {
						sess.heardVoice()
					}
				}
				a.handleFarSegments(segments)
			}
		}
	}
}

// handleMicSegments acts on the stretches of speech the microphone gate has
// finished. Runs on the worker, so it may take as long as a model needs.
func (a *app) handleMicSegments(segments []vad.Segment) {
	for _, seg := range segments {
		// Counted before anything is decided about it. The gate calling a
		// stretch speech is what makes the recording worth keeping; who
		// said it is a separate, stricter question, and answering it in the
		// negative used to throw the whole recording away.
		if sess := a.currentSession(); sess != nil {
			sess.addVoiced(seg.Seconds())
		}

		embed, ok := a.confirmSpeech(seg)
		if !ok {
			continue
		}
		sess := a.openSessionIfIdle(false)
		if sess == nil {
			// Nothing to record into -- yielded, or no model. The next
			// segment may still find a session open, so this drops one
			// reply rather than the rest of the batch.
			continue
		}

		// Live speaker counting: this is what turns a note into a meeting
		// the moment a second person has said enough (see session.noteVoice),
		// and the only thing that keeps the silence timer alive.
		// The reason is logged by escalateLocked, which has the numbers.
		if sess.noteVoice(embed, seg.Seconds()) {
			a.noteSessionKind(sessionMeeting)
			if sess.warnOnce() {
				a.warnAboutRecording()
			}
		}
	}
}

// handleFarSegments is the same for the other side of a call.
func (a *app) handleFarSegments(segments []vad.Segment) {
	for _, seg := range segments {
		if seg.Seconds() < minVoicedSeconds || audio.Level(seg.Samples) < autoSpeechLevel {
			continue
		}
		// Somebody talking on the other end while the user says nothing is a
		// call they are listening to: worth recording, and a conversation by
		// definition.
		sess := a.openSessionIfIdle(true)
		if sess == nil {
			continue
		}

		if sess.noteFarEnd(seg.Seconds()) {
			log.Print("always-on: the far end is talking -- this is a conversation")
			a.noteSessionKind(sessionMeeting)
		}
		// Outside the escalation branch: a session opened as a conversation
		// by openSessionIfIdle above never escalates, because it was one from
		// its first sample. warnOnce is what keeps this to a single banner.
		if sess.warnOnce() {
			a.warnAboutRecording()
		}
	}
}

// stopListening closes the microphone and the tap, and forgets what the gate
// heard. Idempotent.
func (a *app) stopListening() {
	a.stopTap()

	// work is cleared under the lock BEFORE it is closed, and every send is
	// made under that same lock: that is what makes closing it safe while a
	// capture callback may be running.
	a.listen.mu.Lock()
	rec, work, done := a.listen.recorder, a.listen.work, a.listen.workerDone
	was := a.listen.listening
	a.listen.recorder, a.listen.work, a.listen.workerDone = nil, nil, nil
	a.listen.listening = false
	a.listen.preroll, a.listen.farPreroll = nil, nil
	a.listen.mu.Unlock()

	if rec != nil {
		rec.Stop()
		rec.Close()
	}
	if work != nil {
		close(work)
	}
	if done != nil {
		<-done
	}
	if was {
		a.tray.setListening(false)
		log.Print("always-on: stopped listening")
	}
}

// sendToWorker hands one message to the worker, dropping it if the worker is
// behind. Called with a.listen.mu held: the lock is what guarantees the
// channel is not closed underneath the send (see stopListening).
//
// Dropping is the only correct answer to a full queue here. This runs on
// miniaudio's realtime thread, and blocking it to wait for an embedding
// would glitch the very recording being made.
func (l *alwaysOn) sendToWorkerLocked(msg listenMsg) {
	if l.work == nil {
		return
	}
	select {
	case l.work <- msg:
	default:
		l.dropped++
	}
}

// onListenChunk runs on the audio thread, and does only what is safe there:
// keep the pre-roll rolling, hand the chunk to the worker, and write it to
// the open session's file. No model runs here.
//
// The slice is not copied because the recorder allocates a fresh one per
// callback (see audio.Recorder.onData) and nothing else writes to it.
func (a *app) onListenChunk(chunk []float32) {
	l := &a.listen

	l.mu.Lock()
	sess := l.sess
	muted := l.micMuted
	if muted {
		// Silence of the same length, never a shorter file. The two tracks
		// are decoded against one clock, so a microphone file missing the
		// seconds spent muted would drag every word after it ahead of the
		// far end -- and the pre-roll has the same problem, which is why it
		// keeps growing with zeros rather than standing still.
		chunk = make([]float32, len(chunk))
	}
	if sess == nil {
		l.preroll = appendBounded(l.preroll, chunk, prerollSeconds*audio.SampleRate)
	}
	// The gate does not hear a muted microphone either. Otherwise your own
	// voice would keep lastVoiced alive so the session never closed, and
	// noteVoice would go on counting you as a speaker in a recording you
	// asked not to be in. The consequence is intended: while muted, an open
	// session runs down to the silence threshold and closes unless the far
	// end is holding it up, and a closed one does not reopen from the
	// microphone at all.
	if !muted {
		l.sendToWorkerLocked(listenMsg{kind: msgMic, samples: chunk})
	}
	l.mu.Unlock()

	if sess != nil {
		sess.writeMic(chunk)
	}
}

// MicMuted reports whether always-on is writing silence for the user's own
// side.
func (a *app) micMuted() bool {
	a.listen.mu.Lock()
	defer a.listen.mu.Unlock()
	return a.listen.micMuted
}

// toggleListenMute flips the mode-wide microphone mute and reports the new
// state. It deliberately does not touch the session: a recording in progress
// keeps running, it just stops carrying the user's voice.
func (a *app) toggleListenMute() bool {
	a.listen.mu.Lock()
	a.listen.micMuted = !a.listen.micMuted
	muted := a.listen.micMuted
	a.listen.mu.Unlock()
	if muted {
		log.Print("always-on: microphone muted; the far end is still recorded")
	} else {
		log.Print("always-on: microphone unmuted")
	}
	return muted
}

// onFarEndChunk is the same for the system-audio tap: the far end of a call.
func (a *app) onFarEndChunk(chunk []float32) {
	l := &a.listen

	l.mu.Lock()
	sess := l.sess
	if sess == nil {
		l.farPreroll = appendBounded(l.farPreroll, chunk, prerollSeconds*audio.SampleRate)
	}
	l.sendToWorkerLocked(listenMsg{kind: msgFar, samples: chunk})
	l.mu.Unlock()

	if sess != nil {
		sess.writeSystem(chunk, audio.Level(chunk))
	}
}

// appendBounded appends chunk to buf and keeps only the last max samples.
// The tail is copied rather than resliced: reslicing keeps the whole backing
// array alive, which is the leak a buffer fed all day would otherwise be.
func appendBounded(buf, chunk []float32, max int) []float32 {
	buf = append(buf, chunk...)
	if len(buf) <= max {
		return buf
	}
	trimmed := make([]float32, max)
	copy(trimmed, buf[len(buf)-max:])
	return trimmed
}

// confirmSpeech is the second stage, and it returns the fingerprint it
// computed so the caller does not pay for it twice. The VAD has already said
// this stretch is speech; this asks whether it is somebody in the room.
func (a *app) confirmSpeech(seg vad.Segment) ([]float32, bool) {
	if seg.Seconds() < minVoicedSeconds {
		return nil, false
	}
	if audio.Level(seg.Samples) < autoSpeechLevel {
		return nil, false
	}

	// The voice extractor is the strongest filter available, and it is
	// already on disk whenever diarization is set up. Where it is not, the
	// two checks above stand alone rather than blocking the feature on a
	// download the user never asked for -- and the session then has no way
	// to count speakers, so only the far end can make it a conversation.
	if !asr.IsDownloaded(a.modelsDir, diarize.Spec) {
		return nil, true
	}
	e := a.embedders.get(a.modelsDir)
	if e == nil {
		return nil, true
	}
	embed := e.Compute(loudestStretch(seg.Samples, embedSeconds))
	if len(embed) == 0 {
		return nil, false
	}
	return embed, true
}

// loudestStretch returns at most seconds of samples, centred on the loudest
// part of what it was given.
//
// The voice extractor is asked one question -- is this the same person -- and
// three seconds answer it as well as sixty. The length matters for a
// different reason: this runs on the worker, and everything the microphone
// delivers while it runs is queued behind it. A reply can be a minute long
// (the gate has no maximum), and a minute-long embedding is a minute of
// audio arriving with nowhere to go.
func loudestStretch(samples []float32, seconds float64) []float32 {
	want := int(seconds * audio.SampleRate)
	if want <= 0 || len(samples) <= want {
		return samples
	}
	// Loudest by whole windows rather than by sample, so the answer does not
	// hang on one click. A window is a tenth of what is being kept.
	window := want / 10
	if window <= 0 {
		window = 1
	}
	best, bestAt := float32(-1), 0
	for i := 0; i+window <= len(samples); i += window {
		var peak float32
		for _, v := range samples[i : i+window] {
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if peak > best {
			best, bestAt = peak, i
		}
	}
	start := bestAt - want/2
	if start < 0 {
		start = 0
	}
	if start+want > len(samples) {
		start = len(samples) - want
	}
	return samples[start : start+want]
}

// openSessionIfIdle starts recording unless something already is, and hands
// back the session either way. asMeeting is set when the far end opened it,
// where there is nothing to decide: the other side of a call is a second
// party.
//
// Only the worker goroutine calls this, so "is one already open" needs no
// guard beyond the read below -- there is no second opener to race with.
func (a *app) openSessionIfIdle(asMeeting bool) *session {
	a.listen.mu.Lock()
	sess := a.listen.sess
	listening := a.listen.listening
	a.listen.mu.Unlock()
	if sess != nil {
		if asMeeting {
			sess.promote()
			a.noteSessionKind(sessionMeeting)
		}
		return sess
	}
	if !listening || a.listen.isYielded() {
		return nil
	}

	cfg := a.store.Get()
	spec, ok := a.model(cfg)
	if !ok {
		return nil
	}
	if err := os.MkdirAll(a.sessionDir(), 0o755); err != nil {
		log.Printf("always-on: %v", err)
		return nil
	}

	sess, err := newSession(a.sessionDir(), time.Now(), spec, cfg.Language, true)
	if err != nil {
		log.Printf("always-on: %v", err)
		return nil
	}
	if asMeeting {
		sess.promote()
	}

	// Published under the same lock the pre-roll is taken under, and before
	// a single byte of it is written. Between those two used to be a window
	// in which the capture callback saw no session and filed its chunk as
	// the head of the *next* pre-roll -- so the first fraction of a second
	// of every recording turned up at the start of the one after it.
	// Draining the pre-roll, writing it, and publishing the session all
	// happen in one critical section, and the order inside it matters twice
	// over. The pre-roll has to be written before any live chunk, or it
	// lands behind the audio it comes before. And the drain has to be
	// atomic with the publish: there used to be a window between them where
	// the capture callback saw no session and filed its chunk as the head of
	// the *next* pre-roll, so the first moments of every recording turned up
	// at the start of the one after it.
	//
	// The callback is held up for one buffered write while this runs, which
	// is memory and a flush, not a disk wait.
	a.listen.mu.Lock()
	preroll, farPreroll := a.listen.preroll, a.listen.farPreroll
	a.listen.preroll, a.listen.farPreroll = nil, nil
	if len(preroll) > 0 {
		sess.writeMic(preroll)
	}
	a.listen.sess = sess
	a.listen.sessKind = sess.currentKind()
	a.listen.mu.Unlock()

	// The tap may already be running (AlwaysOnTapAlways) or may need
	// starting for this recording; either way the far-end file belongs to
	// the session.
	a.attachSessionSystemAudio(cfg, sess, farPreroll)

	// The clock in the menu bar has to move, or "recording" reads as stuck.
	sess.stopTicker = make(chan struct{})
	go a.tickMeetingClock(sess.stopTicker)

	a.tray.startedMeeting(sess.start)
	if a.onMeetingState != nil {
		a.onMeetingState(true)
	}
	log.Printf("always-on: recording to %s", sess.micPath)
	go a.models.warm(spec, asr.ModelDir(a.modelsDir, spec), cfg.Language)
	return sess
}

// attachSessionSystemAudio gives the session somewhere to put the far end,
// starting the tap first if it is not already running.
func (a *app) attachSessionSystemAudio(cfg settings.Settings, sess *session, farPreroll []float32) {
	if cfg.AlwaysOnSystemAudio != settings.AlwaysOnTapAlways {
		a.startTap()
	}
	a.listen.mu.Lock()
	running := a.listen.tapRunning
	a.listen.mu.Unlock()
	if !running {
		return
	}

	w, err := audio.NewWAVWriter(sess.sysPath)
	if err != nil {
		// Not fatal: a conversation recorded from the microphone alone is
		// still a conversation.
		log.Printf("always-on: system audio: %v", err)
		return
	}
	sess.attachSystem(w, sess.sysPath, true)
	if len(farPreroll) > 0 {
		sess.writeSystem(farPreroll, 0)
	}
}

// syncTap opens or closes the system-audio tap to match the setting. In
// AlwaysOnTapAlways it runs for as long as listening does, which is what
// catches a call where the other side speaks first; otherwise it only runs
// while a recording does.
func (a *app) syncTap(cfg settings.Settings) {
	a.listen.mu.Lock()
	running, hasSession := a.listen.tapRunning, a.listen.sess != nil
	a.listen.mu.Unlock()

	wantAlways := cfg.AlwaysOnSystemAudio == settings.AlwaysOnTapAlways
	switch {
	case wantAlways && !running:
		a.startTap()
	case !wantAlways && running && !hasSession:
		a.stopTap()
	}
}

// startTap opens the system-audio tap and its own voice-activity gate. The
// gate is handed to the worker rather than kept here: one goroutine owns
// every gate, which is what makes them lock-free.
func (a *app) startTap() {
	a.listen.mu.Lock()
	running, listening := a.listen.tapRunning, a.listen.listening
	a.listen.mu.Unlock()
	if running || !listening {
		return
	}

	gate, err := vad.New(asr.ModelDir(a.modelsDir, vad.Spec), vad.DefaultConfig())
	if err != nil {
		log.Printf("always-on: system audio gate: %v", err)
		return
	}
	if err := systemaudio.Start(a.onFarEndChunk); err != nil {
		gate.Close()
		a.noteSystemAudioFailure(err)
		return
	}

	a.listen.mu.Lock()
	a.listen.tapRunning = true
	a.listen.sendToWorkerLocked(listenMsg{kind: msgFarGateOpen, gate: gate})
	handed := a.listen.work != nil
	a.listen.mu.Unlock()
	if !handed {
		// Listening stopped underneath us; nobody will ever close it.
		gate.Close()
	}
	a.clearSystemAudioFailure()
}

func (a *app) stopTap() {
	a.listen.mu.Lock()
	running := a.listen.tapRunning
	a.listen.tapRunning = false
	a.listen.farPreroll = nil
	a.listen.sendToWorkerLocked(listenMsg{kind: msgFarGateClose})
	a.listen.mu.Unlock()
	if !running {
		return
	}
	systemaudio.Stop()
}

// System audio failing is a silent feature: the tap simply never delivers,
// and until now the only trace was a log line. It fails for exactly one
// reason in practice -- the Screen Recording permission, which macOS drops
// every time the app is signed with a different key -- so it is worth
// saying out loud, once, and worth showing in Settings for as long as it
// lasts.
var (
	sysAudioMu     sync.Mutex
	sysAudioErr    string
	sysAudioToldAt bool
)

func (a *app) noteSystemAudioFailure(err error) {
	sysAudioMu.Lock()
	first := sysAudioErr == ""
	sysAudioErr = err.Error()
	tell := !sysAudioToldAt
	sysAudioToldAt = true
	sysAudioMu.Unlock()

	if first {
		log.Printf("always-on: system audio: %v", err)
	}
	if tell {
		notifyPane("Voxlog cannot hear system audio. Grant Screen Recording in Settings.", ui.PaneSettings)
	}
}

func (a *app) clearSystemAudioFailure() {
	sysAudioMu.Lock()
	had := sysAudioErr != ""
	sysAudioErr = ""
	sysAudioMu.Unlock()
	if had {
		log.Print("always-on: system audio working again")
	}
}

// SystemAudioError is what Settings shows, or "" when the tap is fine. Read
// by the permissions poll the Settings pane already runs.
func SystemAudioError() string {
	sysAudioMu.Lock()
	defer sysAudioMu.Unlock()
	return sysAudioErr
}

// noteSessionKind records an escalation for the supervisor, which reads the
// kind on every tick to pick the right silence threshold.
func (a *app) noteSessionKind(kind string) {
	a.listen.mu.Lock()
	a.listen.sessKind = kind
	a.listen.mu.Unlock()
}

// closeSessionIfDone ends the recording once the room has been quiet for
// long enough -- a shorter pause for a note, which is one thought, than for
// a conversation, which has pauses in it.
func (a *app) closeSessionIfDone(cfg settings.Settings) {
	a.listen.mu.Lock()
	sess, kind := a.listen.sess, a.listen.sessKind
	a.listen.mu.Unlock()
	if sess == nil {
		return
	}

	gap := time.Duration(noteGapSeconds(cfg) * float64(time.Second))
	if kind == sessionMeeting {
		gap = time.Duration(splitMinutes(cfg) * float64(time.Minute))
	}
	// A recording the gate has not yet heard any real speech in is a guess
	// made on its first impression. It gets seconds to be justified, not
	// the minute a real note is given to finish a thought. Keyed on what
	// was heard rather than on who was recognised: recognition has a
	// loudness floor, and a quiet speaker was being given twelve seconds to
	// say something the app would then throw away anyway.
	if sess.voicedSeconds() < minVoicedToKeep {
		gap = provisionalGrace
	}
	if sess.quietFor() < gap && time.Since(sess.start) < maxSessionHours*time.Hour {
		return
	}
	a.closeSession()
}

func noteGapSeconds(cfg settings.Settings) float64 {
	if cfg.AlwaysOnNoteGapSeconds > 0 {
		return cfg.AlwaysOnNoteGapSeconds
	}
	return 60
}

func splitMinutes(cfg settings.Settings) float64 {
	if cfg.AlwaysOnSplitMinutes > 0 {
		return cfg.AlwaysOnSplitMinutes
	}
	return 5
}

// closeSession ends the recording in progress and routes it by what it
// turned out to be. Safe to call with nothing open.
func (a *app) closeSession() {
	a.listen.mu.Lock()
	sess := a.listen.sess
	a.listen.sess, a.listen.sessKind = nil, ""
	a.listen.mu.Unlock()
	if sess == nil {
		return
	}

	cfg := a.store.Get()
	if cfg.AlwaysOnSystemAudio != settings.AlwaysOnTapAlways {
		a.stopTap()
	}

	if sess.stopTicker != nil {
		close(sess.stopTicker)
	}

	micPath, sysPath, sysVoiced := sess.closeFiles()
	// The file's own length, not the wall clock: closeFiles has just cut the
	// silence off the end, and a History row claiming a minute for a
	// twenty-second recording is a row that disagrees with its own player.
	seconds := audio.DurationSeconds(micPath)
	if seconds <= 0 {
		seconds = time.Since(sess.start).Seconds()
	}
	kind := sess.currentKind()

	a.tray.stoppedMeeting()
	if a.onMeetingState != nil {
		a.onMeetingState(false)
	}
	log.Printf("always-on: %s of %.0fs ended (%.1fs of far-end audio)", kind, seconds, sysVoiced)

	// Reported per recording, not once per run: dropping is how the gate
	// loses track of a conversation it is in the middle of timing, so the
	// number belongs beside the recording it cost.
	a.listen.mu.Lock()
	dropped := a.listen.dropped
	a.listen.dropped = 0
	a.listen.mu.Unlock()
	if dropped > 0 {
		log.Printf("always-on: the gate went deaf for %.1fs of this recording -- the worker was behind by %d chunks",
			float64(dropped)*chunkSeconds, dropped)
	}

	// Whether this was worth recording is decided by how much speech the
	// gate heard, not by whether the second stage recognised a voice.
	//
	// It used to be the second stage, and that stage has a loudness floor
	// under it: a quiet sentence, or one said across the room, never
	// cleared it, and the whole recording -- file, row and all -- was
	// deleted with nothing said about it. Deleting what somebody actually
	// said is the worst thing an always-on feature can do, and it is
	// unrecoverable. The gate is the right authority: it is a speech model,
	// it has already been asked this question, and where it is wrong the
	// transcript comes back empty and fileAutoNote drops the audio anyway.
	if voiced := sess.voicedSeconds(); voiced < minVoicedToKeep {
		log.Printf("always-on: %.0fs with %.1fs of speech in it, discarded", seconds, voiced)
		removeQuietly(micPath, sysPath)
		return
	}

	// Too short to be anything: delete rather than file. A three-second file
	// in History is noise about noise.
	if seconds < minAutoRecordingSeconds {
		removeQuietly(micPath, sysPath)
		return
	}

	// A file the writer never got anything into -- or never closed, which
	// looks the same to every reader. Filing it would put a row in History
	// pointing at silence, and queue a decode that can only come back
	// empty.
	if audio.DurationSeconds(micPath) <= 0 {
		log.Printf("always-on: %s has no audio in it, discarded", micPath)
		removeQuietly(micPath, sysPath)
		return
	}

	// The far-end file counts as a second party only when something actually
	// came out of it -- otherwise it is silence, and decoding it buys a
	// second pass and a heading over nothing.
	if sysVoiced < minVoicedSeconds {
		removeQuietly(sysPath)
		sysPath = ""
	}

	if kind == sessionMeeting {
		a.fileAutoMeeting(sess, micPath, sysPath, seconds)
		return
	}
	a.fileAutoNote(sess, micPath, sysPath, seconds)
}

// fileAutoMeeting hands the recording to the ordinary meeting path: same
// history row, same transcription queue, same turns and speaker identity.
func (a *app) fileAutoMeeting(sess *session, micPath, sysPath string, seconds float64) {
	rec := history.Meeting{
		Start:            sess.start,
		RecordingSeconds: seconds,
		AudioPath:        micPath,
		SystemAudioPath:  sysPath,
		AutoStarted:      true,
	}
	if err := a.meetings.Append(rec); err != nil {
		log.Printf("always-on: history append: %v", err)
		return
	}
	go a.sweepRecordings()
	ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
	a.transcribeMeetingEntry(rec, sess.spec, sess.language)
}

// fileAutoNote transcribes a one-voice recording and files it in History,
// beside dictations.
//
// It is emphatically NOT emitted: output.Emit pastes into whatever app is
// frontmost, and pasting a sentence the user muttered to themselves into the
// document they are writing is the worst thing this feature could do.
func (a *app) fileAutoNote(sess *session, micPath, sysPath string, seconds float64) {
	// The far end has no business in a note. If the tap caught anything at
	// all, this was misjudged as a note and the meeting path would have
	// taken it, so the file is only ever silence here.
	removeQuietly(sysPath)

	// The row is written BEFORE the decode, not after it, for the same
	// reason a meeting's is: a transcript can take minutes, the app can be
	// quit in the middle of one, and a recording that exists only as a file
	// on disk is one the orphan sweep will adopt as a meeting. Written now,
	// it shows up in History the moment the recording ends, marked as
	// transcribing, and the text lands in it later.
	at := sess.start
	if err := a.hist.Append(history.Entry{
		Timestamp:        at,
		Text:             "",
		RecordingSeconds: seconds,
		AudioPath:        micPath,
		AutoStarted:      true,
	}); err != nil {
		log.Printf("always-on: history append: %v", err)
		return
	}
	ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)

	key := at.Format(time.RFC3339Nano)
	a.queue.submit("Note, "+at.Format("15:04"), key, seconds, func(yield func()) {
		started := time.Now()
		text := a.transcribeWholeFile(sess.spec, sess.language, micPath, yield)
		if text == "" {
			// The row stays: it is the only evidence the machine recorded
			// something here, and a silent deletion is exactly the behaviour
			// that makes an always-on feature impossible to trust. The audio
			// goes, because nothing will ever be got out of it.
			log.Printf("always-on: nothing transcribed from %s", micPath)
			removeQuietly(micPath)
			if err := a.hist.Update(at, func(e *history.Entry) {
				e.AudioPath = ""
				e.DurationSeconds = time.Since(started).Seconds()
			}); err != nil {
				log.Printf("always-on: history update: %v", err)
			}
			ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
			return
		}
		if err := a.hist.Update(at, func(e *history.Entry) {
			e.Text = text
			e.DurationSeconds = time.Since(started).Seconds()
		}); err != nil {
			log.Printf("always-on: history update: %v", err)
			return
		}
		ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
		notifyPane("Noted something you said.", ui.PaneHistory)
		go a.classifyForTasks(history.KindDictation, key, text)
	})
}

// transcribeWholeFile decodes a recording block by block into one string.
// No diarization: a note is one voice by definition -- that is what made it
// a note.
func (a *app) transcribeWholeFile(spec asr.ModelSpec, language, path string, yield func()) string {
	// Only the speech, where the gate can say where the speech is. Handing
	// the recognizer the empty room between sentences is what produced both
	// halves of the complaint this came from: most of what was said missing,
	// and a paragraph of invented text where nothing was said at all.
	if spans, ok := a.speechSpans(path); ok {
		if len(spans) == 0 {
			log.Printf("always-on: no speech found in %s", path)
			return ""
		}
		return a.transcribeSpans(spec, language, path, spans, yield)
	}

	var parts []string
	err := audio.ReadBlocks(path, func(block []float32) error {
		if text := a.transcribeBlock(spec, language, block, nil, false); text != "" {
			parts = append(parts, text)
		}
		yield()
		return nil
	})
	if err != nil {
		log.Printf("always-on: reading %s: %v", path, err)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// transcribeSpans decodes the marked stretches of a recording, in order.
// Each one is split into blocks of its own, so a long stretch still hands
// over to a waiting dictation between blocks.
func (a *app) transcribeSpans(spec asr.ModelSpec, language, path string, spans []speechSpan, yield func()) string {
	var parts []string
	for _, span := range spans {
		samples, err := audio.ReadRange(path,
			float64(span.start)/audio.SampleRate, float64(span.end)/audio.SampleRate)
		if err != nil {
			log.Printf("always-on: reading %s: %v", path, err)
			continue
		}
		for len(samples) > 0 {
			cut := len(samples)
			if cut > audio.BlockSamples {
				cut = audio.CutPoint(samples, audio.BlockSamples)
			}
			if text := a.transcribeBlock(spec, language, samples[:cut], nil, false); text != "" {
				parts = append(parts, text)
			}
			samples = samples[cut:]
			yield()
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func removeQuietly(paths ...string) {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("always-on: removing %s: %v", path, err)
		}
	}
}

// currentSession is the recording in progress, or nil. Unlike
// openSessionIfIdle it starts nothing: callers that have something to say
// about a recording, rather than something to record, use this.
func (a *app) currentSession() *session {
	a.listen.mu.Lock()
	defer a.listen.mu.Unlock()
	return a.listen.sess
}

// sessionPathsInUse is what a retention sweep must not delete: the files the
// open session is still writing to.
func (a *app) sessionPathsInUse() (string, string) {
	a.listen.mu.Lock()
	sess := a.listen.sess
	a.listen.mu.Unlock()
	if sess == nil {
		return "", ""
	}
	return sess.paths()
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
			notifyPane("Could not download the voice-activity model, so always-on listening is off.", ui.PaneSettings)
			return
		}
		log.Print("always-on: voice-activity model ready")
	}()
}

// sweepAutoRecordings deletes auto recordings that never produced a
// transcript and are older than the retention window.
//
// Retention stops being optional once the app listens all day. A recording
// the gate opened and the transcript never justified is a guess that did not
// pay off: the audio goes, the row stays (it is one line, and it is the only
// evidence the machine was listening at that hour). Anything with a
// transcript, and anything the user started by hand, is untouched.
func (a *app) sweepAutoRecordings() {
	cfg := a.store.Get()
	if !cfg.AlwaysOn || cfg.AlwaysOnRetentionHours <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(cfg.AlwaysOnRetentionHours * float64(time.Hour)))
	inUse := a.recordingsInUse()

	meetings, err := a.meetings.All()
	if err != nil {
		log.Printf("always-on sweep: %v", err)
		return
	}
	for _, m := range meetings {
		if !m.AutoStarted || m.Text != "" || m.Start.After(cutoff) {
			continue
		}
		if inUse[m.AudioPath] || inUse[m.SystemAudioPath] {
			continue
		}
		if m.AudioPath == "" && m.SystemAudioPath == "" {
			continue
		}
		removeQuietly(m.AudioPath, m.SystemAudioPath)
		if err := a.meetings.Update(m.Start, func(e *history.Meeting) {
			e.AudioPath, e.SystemAudioPath = "", ""
		}); err != nil {
			log.Printf("always-on sweep: clearing the audio path: %v", err)
		}
		log.Printf("always-on sweep: dropped the audio of %s, never transcribed",
			m.Start.Format(time.RFC3339))
	}
}

// yieldMicToUser hands the microphone to a dictation, at once rather than on
// the supervisor's next tick: two seconds of the gate still listening is two
// seconds of it reacting to the very words being dictated.
//
// MUST NOT be called with a.mu held. It waits for the worker to drain, and
// the worker is not allowed to want a.mu for exactly that reason (see the
// yielded flag).
func (a *app) yieldMicToUser() {
	a.listen.mu.Lock()
	already := a.listen.yielded
	a.listen.yielded = true
	a.listen.mu.Unlock()
	if already {
		return
	}
	a.closeSession()
	a.stopListening()
}

// reclaimMic lets always-on have the microphone back.
//
// Listening is not restarted here -- the dictation's own recorder is still
// being torn down, and racing it for the device buys nothing -- but the
// supervisor is woken so that it happens on the next instant rather than on
// the next tick.
func (a *app) reclaimMic() {
	a.listen.mu.Lock()
	a.listen.yielded = false
	a.listen.mu.Unlock()
	a.listen.nudge()
}

func (l *alwaysOn) isYielded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.yielded
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

// toggleListenPause is what the menu item does: "not right now", without
// turning the feature off. Reports the state it landed in.
func (a *app) toggleListenPause() bool {
	paused := !a.listen.isPaused()
	a.listen.setPaused(paused)
	if paused {
		a.closeSession()
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

// listenMuteLabel is the same for the mode-wide microphone mute. Sitting one
// line under Pause, the names alone cannot say how the two differ -- that is
// what the tooltips are for: pause stops everything, mute keeps recording the
// far end of a call and drops only your side.
func listenMuteLabel(muted bool) string {
	if muted {
		return "Unmute my microphone"
	}
	return "Mute my microphone"
}

const (
	listenPauseTip = "Stop listening entirely -- neither you nor the other side is recorded -- until you resume it"
	listenMuteTip  = "Keep listening but write silence for your side. A call you are sitting quietly through is still recorded."
)
