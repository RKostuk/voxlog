package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/history"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/systemaudio"
	"voxlog-go/internal/ui"
)

// meetingsDirName is where meeting audio is kept, inside the transcripts
// directory. Together with the day files, so one retention sweep and one
// backup cover a call and the transcript it produced.
const meetingsDirName = "Recordings"

// trayTickInterval is how often the elapsed time in the menu bar is redrawn.
//
// One second, because a clock that moves is the difference between "it is
// recording" and "it might be stuck": at five seconds the number sat still
// long enough to read as frozen, which is exactly the doubt this indicator
// exists to remove. The cost is one SetTitle per second while a recording
// runs, and none at all when nothing is.
const trayTickInterval = time.Second

// meeting is a call being recorded, from the key that started it to the key
// that stops it.
//
// Unlike a dictation it never holds its audio in memory: an hour is 230MB
// resident and gone entirely if the app dies, so both channels are written to
// disk as they arrive and decoded from there afterwards.
type meeting struct {
	recorder *audio.Recorder
	start    time.Time
	spec     asr.ModelSpec
	language string

	// mu guards the writers: the microphone and the system tap deliver on two
	// different threads.
	mu        sync.Mutex
	micWAV    *audio.WAVWriter
	sysWAV    *audio.WAVWriter
	micPath   string
	sysPath   string
	sysVoiced float64

	// muted silences the microphone track for the rest of this recording.
	// Silence is written rather than dropped: the two tracks are decoded
	// against one clock, and a mic file short by the length of the mute would
	// put every word after it ahead of the other side of the call.
	//
	// Per recording, deliberately: it is the "I am about to cough" button, not
	// a setting, so it lives on the meeting and dies with it.
	muted bool

	sysRunning bool
	stopTicker chan struct{}

	// lastVoiced is when either side last carried something loud enough to
	// be speech. Kept for the same reason a session keeps it, and read by
	// nothing else today: a meeting the user started ends when they say so.
	// Guarded by mu.
	lastVoiced time.Time
}

// meetingPaths builds the two file names for a meeting starting now.
func meetingPaths(dir string, at time.Time) (mic, system string) {
	stamp := at.Format("2006-01-02-150405")
	return filepath.Join(dir, stamp+"-mic.wav"), filepath.Join(dir, stamp+"-system.wav")
}

// startMeeting begins recording a call. a.mu must be held.
// startMeeting records a call the user asked for. Always-on's own
// recordings do not come through here -- it owns the microphone and writes
// its own session (see alwayson.go), because what it is recording is not
// known to be a meeting until somebody else speaks.
func (a *app) startMeeting() {
	// The backlog gives the machine back before the first sample is recorded,
	// not after the call: an old meeting being re-read must never be why this
	// one decodes slowly.
	a.stopBackfillLocked()

	cfg := a.store.Get()
	spec, ok := a.model(cfg)
	if !ok {
		return
	}

	at := time.Now()
	dir := filepath.Join(a.hist.Dir(), meetingsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("meeting: %v", err)
		notify("Could not create the folder for meeting recordings.")
		return
	}
	micPath, sysPath := meetingPaths(dir, at)

	micWAV, err := audio.NewWAVWriter(micPath)
	if err != nil {
		log.Printf("meeting: %v", err)
		notify("Could not start the meeting recording.")
		return
	}

	m := &meeting{
		start:      at,
		spec:       spec,
		language:   cfg.Language,
		micWAV:     micWAV,
		micPath:    micPath,
		lastVoiced: at,
	}

	rec, err := audio.NewRecorder(cfg.InputDevice, cfg.MicGain)
	if err != nil {
		log.Printf("meeting recorder init: %v", err)
		notify("Could not access the microphone.")
		micWAV.Close()
		os.Remove(micPath)
		return
	}
	m.recorder = rec

	// StartUnbuffered: the samples are on their way to disk, and a second copy
	// in memory is the thing this whole design avoids.
	if err := rec.StartUnbuffered(func(chunk []float32) {
		voiced := audio.Level(chunk) >= systemVoicedThreshold
		m.mu.Lock()
		if m.muted {
			chunk = make([]float32, len(chunk)) // same length, no sound
		} else if voiced {
			m.lastVoiced = time.Now()
		}
		err := m.micWAV.Write(chunk)
		m.mu.Unlock()
		if err != nil {
			log.Printf("meeting: writing microphone audio: %v", err)
		}
	}); err != nil {
		log.Printf("meeting recorder start: %v", err)
		notify("Could not start recording.")
		rec.Close()
		micWAV.Close()
		os.Remove(micPath)
		return
	}

	// A meeting always tries for system audio: the other side of the call is
	// most of what a call is. The CaptureSystemAudio setting is about plain
	// dictation, not this.
	sysWAV, err := audio.NewWAVWriter(sysPath)
	if err != nil {
		log.Printf("meeting: %v", err)
	} else if err := systemaudio.Start(func(chunk []float32) {
		level := audio.Level(chunk)
		m.mu.Lock()
		if level >= systemVoicedThreshold {
			m.sysVoiced += float64(len(chunk)) / audio.SampleRate
			m.lastVoiced = time.Now()
		}
		err := m.sysWAV.Write(chunk)
		m.mu.Unlock()
		if err != nil {
			log.Printf("meeting: writing system audio: %v", err)
		}
	}); err != nil {
		// Not fatal: a meeting recorded from the microphone alone is still a
		// meeting, and the alternative is refusing to record the call at all.
		log.Printf("meeting system audio: %v", err)
		notify("Recording the meeting from the microphone only: " + err.Error())
		sysWAV.Close()
		os.Remove(sysPath)
	} else {
		m.sysWAV, m.sysPath, m.sysRunning = sysWAV, sysPath, true
	}

	m.stopTicker = make(chan struct{})
	go a.tickMeetingClock(m.stopTicker)

	a.meeting = m
	a.tray.startedMeeting(at)
	if a.onMeetingState != nil {
		a.onMeetingState(true)
	}
	log.Printf("meeting: recording to %s", micPath)

	go a.models.warm(spec, asr.ModelDir(a.modelsDir, spec), cfg.Language)
}

// tickMeetingClock keeps the elapsed time in the menu bar moving.
func (a *app) tickMeetingClock(stop <-chan struct{}) {
	ticker := time.NewTicker(trayTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			a.tray.refresh()
		}
	}
}

// stopMeeting ends the call, writes it to history, and queues the transcript
// if the settings ask for one now. a.mu must be held.
func (a *app) stopMeeting() {
	m := a.meeting
	a.meeting = nil
	cfg := a.store.Get()

	close(m.stopTicker)
	m.recorder.Stop()
	m.recorder.Close()
	if m.sysRunning {
		systemaudio.Stop()
	}

	m.mu.Lock()
	if err := m.micWAV.Close(); err != nil {
		log.Printf("meeting: closing the microphone recording: %v", err)
	}
	if m.sysWAV != nil {
		if err := m.sysWAV.Close(); err != nil {
			log.Printf("meeting: closing the system recording: %v", err)
		}
	}
	voiced := m.sysVoiced
	m.mu.Unlock()

	a.tray.stoppedMeeting()
	if a.onMeetingState != nil {
		a.onMeetingState(false)
	}

	seconds := time.Since(m.start).Seconds()
	// The system side counts as a second party only when something actually
	// came out of it -- a call with nothing playing has one speaker, and
	// splitting it would buy a second decode pass and a heading over nothing.
	sysPath := m.sysPath
	if voiced < minVoicedSeconds {
		sysPath = ""
	}
	log.Printf("meeting: %.0fs recorded, %.1fs of system audio", seconds, voiced)

	rec := history.Meeting{
		Start:            m.start,
		RecordingSeconds: seconds,
		AudioPath:        m.micPath,
		SystemAudioPath:  sysPath,
	}
	if err := a.meetings.Append(rec); err != nil {
		log.Printf("meeting: history append: %v", err)
	}
	go a.sweepRecordings()

	if cfg.MeetingTranscribe != settings.MeetingTranscribeStop {
		// Nothing decodes it now, so whether the audio stays is decided on the
		// spot rather than waiting for a transcript that may never come.
		a.discardMeetingAudioIfDisabled(rec)
		notifyPane("Meeting recorded. Transcribe it from Meetings.", "meetings")
		ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
		return
	}
	a.transcribeMeetingEntry(rec, m.spec, m.language)
}

// transcribeMeetingEntry queues a meeting's audio for decoding and attaches
// the transcript to its history entry when it arrives. Shared by the automatic
// path and the Transcribe action in the History window.
func (a *app) transcribeMeetingEntry(m history.Meeting, spec asr.ModelSpec, language string) {
	separate := a.store.Get().SeparateSpeakers && m.SystemAudioPath != ""

	a.queue.submit("Meeting, "+m.Start.Format("15:04"), m.Start.Format(time.RFC3339Nano), m.RecordingSeconds, func(yield func()) {
		started := time.Now()
		turns, err := a.transcribeFilesTurns(spec, language, m.AudioPath, m.SystemAudioPath, separate, yield, nil)
		if err != nil {
			log.Printf("meeting: transcription stopped: %v", err)
			return
		}
		// One pass over the whole meeting, not one per block: this is what
		// makes "Speaker 2" in the last minute the same person as in the
		// first (see speakers.go).
		linkSpeakers(turns)

		text := renderTurns(turns, nil)
		if text == "" {
			log.Printf("meeting: nothing transcribed from %s", m.AudioPath)
			notify("The meeting recording produced no transcript.")
			return
		}

		// Turns before text. A crash between the two leaves a meeting whose
		// replies are all there but whose one-string transcript is missing,
		// which the window can still render; the other order would leave a
		// transcript that claims a structure nothing has.
		if err := a.meetings.ReplaceTurns(m.Start, meetingSpeakers(turns), historyTurns(turns), turnsSchemaVersion); err != nil {
			log.Printf("meeting: saving turns: %v", err)
		} else if unsure, err := a.meetings.IdentifySpeakers(m.Start); err != nil {
			log.Printf("meeting: identifying speakers: %v", err)
		} else if len(unsure) > 0 {
			// Not a notification: a "does this sound like Ірина?" banner during
			// the working day would be an interruption for something that can
			// wait in the Voices pane until the user goes looking.
			log.Printf("meeting: %d speaker(s) look familiar but not certainly so", len(unsure))
		}

		took := time.Since(started).Seconds()
		if err := a.meetings.Update(m.Start, func(e *history.Meeting) {
			e.Text = text
			e.DurationSeconds = took
		}); err != nil {
			log.Printf("meeting: attaching the transcript: %v", err)
			return
		}
		go a.classifyForTasks(history.KindMeeting, m.Start.Format(time.RFC3339Nano), text)
		go a.summarizeMeeting(m.Start, text)
		a.discardMeetingAudioIfDisabled(m)
		ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
		notifyPane("Meeting transcript ready.", "meetings")
	})
}

// transcribeStoredMeeting is the History window's Transcribe action: decode a
// meeting recorded earlier, whose audio is still on disk. Identified by
// timestamp, which is what the window has to hand.
func (a *app) transcribeStoredMeeting(at time.Time) error {
	meetings, err := a.meetings.All()
	if err != nil {
		return err
	}
	for _, m := range meetings {
		if !m.Start.Equal(at) {
			continue
		}
		if m.AudioPath == "" {
			return errors.New("this recording is no longer on disk")
		}
		if _, err := os.Stat(m.AudioPath); err != nil {
			return errors.New("this recording is no longer on disk")
		}
		cfg := a.store.Get()
		spec, ok := modelForSettings(cfg)
		if !ok || !asr.IsDownloaded(a.modelsDir, spec) {
			return errors.New("no usable model is selected")
		}
		a.transcribeMeetingEntry(m, spec, cfg.Language)
		return nil
	}
	return errors.New("that recording is not in the history")
}

// discardMeetingAudioIfDisabled removes a meeting's recording when
// KeepMeetingAudio is off, clearing the paths on the record so the history
// entry stops pointing at a file that is no longer there. The files are
// written while the meeting runs regardless -- an hour is too much to hold in
// memory just to honour the setting -- so this is only about cleaning up
// afterwards. A failure here must not take the history entry down with it,
// so it is logged and swallowed like the dictation write path.
func (a *app) discardMeetingAudioIfDisabled(m history.Meeting) {
	if a.store.Get().KeepMeetingAudio {
		return
	}

	for _, path := range []string{m.AudioPath, m.SystemAudioPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("meeting: removing %s: %v", path, err)
		}
	}
	if err := a.meetings.Update(m.Start, func(e *history.Meeting) {
		e.AudioPath, e.SystemAudioPath = "", ""
	}); err != nil {
		log.Printf("meeting: clearing the audio path: %v", err)
	}
}

// toggleMeeting is what the meeting key does.
func (a *app) toggleMeeting() {
	a.mu.Lock()
	running := a.meeting != nil
	a.mu.Unlock()

	if running {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.stopMeeting()
		return
	}

	// A session always-on opened is closed and filed first, outside the
	// lock: the user pressing the key means "record this deliberately", and
	// two recordings of the same room would be two of everything.
	a.closeSession()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.meeting != nil {
		// Raced with another press; the second one is the stop.
		a.stopMeeting()
		return
	}
	a.startMeeting()
}

// meetingElapsed is how long the meeting being recorded has been running,
// and false when there is none.
func (a *app) meetingElapsed() (bool, time.Duration) {
	a.mu.Lock()
	m := a.meeting
	a.mu.Unlock()
	if m != nil {
		return true, time.Since(m.start)
	}

	// A recording always-on opened counts as well. With the manual recorder
	// hidden while listening is on, this banner is the only "recording now"
	// control left on screen, and it must not go blank the moment the
	// feature that does the recording is the one doing it.
	a.listen.mu.Lock()
	sess := a.listen.sess
	a.listen.mu.Unlock()
	if sess == nil {
		return false, 0
	}
	return true, time.Since(sess.start)
}

// meetingMuted reports whether the meeting being recorded has its microphone
// muted, for the menu line and the menu bar glyph.
func (a *app) meetingMuted() bool {
	a.mu.Lock()
	m := a.meeting
	a.mu.Unlock()
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.muted
}

// toggleMeetingMute silences (or unsilences) the microphone side of the
// meeting being recorded, and reports the state it landed in. False when
// there is no meeting to mute.
func (a *app) toggleMeetingMute() bool {
	a.mu.Lock()
	m := a.meeting
	a.mu.Unlock()
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.muted = !m.muted
	return m.muted
}

// stopMeetingIfRunning backs the tray menu item, which must do nothing at all
// when there is no meeting.
func (a *app) stopMeetingIfRunning() {
	// Always-on's own recording is a recording too: the Stop button, the
	// menu item and the Overview banner all mean "stop what is being
	// recorded", and the user should not have to know which of the two
	// mechanisms opened the file.
	a.closeSession()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.meeting != nil {
		a.stopMeeting()
	}
}

// adoptOrphanedMeetings picks up recordings left behind by a crash or a quit
// mid-call: the audio is on disk from the first chunk, so the only thing
// missing is the history entry that would have been written at the end.
func (a *app) adoptOrphanedMeetings() {
	dir := filepath.Join(a.hist.Dir(), meetingsDirName)
	files, err := filepath.Glob(filepath.Join(dir, "*-mic.wav"))
	if err != nil || len(files) == 0 {
		return
	}

	meetings, err := a.meetings.All()
	if err != nil {
		log.Printf("meeting: cannot check for orphaned recordings: %v", err)
		return
	}
	known := map[string]bool{}
	for _, m := range meetings {
		if m.AudioPath != "" {
			known[m.AudioPath] = true
		}
	}
	// Notes recorded by always-on live in the same folder and are named the
	// same way, but they are history entries, not meetings. Without this
	// they get adopted as meetings on the next launch -- which is exactly
	// what happened to the first two notes the feature ever recorded.
	entries, err := a.hist.AllEntries()
	if err != nil {
		log.Printf("meeting: cannot check notes for orphaned recordings: %v", err)
		return
	}
	for _, e := range entries {
		if e.AudioPath != "" {
			known[e.AudioPath] = true
		}
	}

	for _, micPath := range files {
		if known[micPath] {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02-150405",
			strings.TrimSuffix(filepath.Base(micPath), "-mic.wav"), time.Local)
		if err != nil {
			continue // not one of ours
		}
		r, err := audio.OpenWAV(micPath)
		if err != nil {
			log.Printf("meeting: ignoring %s: %v", micPath, err)
			continue
		}
		seconds := r.Seconds()
		r.Close()
		if seconds < minVoicedSeconds {
			os.Remove(micPath) // a meeting that never got going
			continue
		}

		sysPath := strings.TrimSuffix(micPath, "-mic.wav") + "-system.wav"
		if _, err := os.Stat(sysPath); err != nil {
			sysPath = ""
		}
		rec := history.Meeting{
			Start:            at,
			RecordingSeconds: seconds,
			AudioPath:        micPath,
			SystemAudioPath:  sysPath,
		}
		if err := a.meetings.Append(rec); err != nil {
			log.Printf("meeting: adopting %s: %v", micPath, err)
			continue
		}
		log.Printf("meeting: adopted %s (%.0fs) left behind by a previous run", micPath, seconds)
	}
}
