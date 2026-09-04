package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/history"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/vad"
	"voxlog-go/internal/voiceid"
)

func TestExcludedMatchesByNameCaseInsensitively(t *testing.T) {
	list := []string{"1Password", " Zoom "}
	for _, app := range []string{"1Password", "1password", "Zoom"} {
		if !excluded(app, list) {
			t.Errorf("excluded(%q) = false, want true", app)
		}
	}
	for _, app := range []string{"", "Safari", "Zoom Rooms"} {
		if excluded(app, list) {
			t.Errorf("excluded(%q) = true, want false", app)
		}
	}
}

func TestExcludedWithNoListNeverExcludes(t *testing.T) {
	if excluded("Safari", nil) {
		t.Fatal("an empty exclusion list must not exclude anything")
	}
}

func TestPauseStopsListeningUntilResumed(t *testing.T) {
	a := &app{}
	if a.listen.isPaused() {
		t.Fatal("listening starts unpaused")
	}
	if !a.toggleListenPause() {
		t.Fatal("first toggle should pause")
	}
	if !a.listen.isPaused() {
		t.Fatal("pause did not stick")
	}
	if a.toggleListenPause() {
		t.Fatal("second toggle should resume")
	}
	// The label says what the click will do, not what the state is.
	if listenPauseLabel(true) != "Resume listening" || listenPauseLabel(false) != "Pause listening" {
		t.Fatal("the menu label is backwards")
	}
}

func TestPendingIsConsumedOnce(t *testing.T) {
	// The audio threads raise these flags and the supervisor acts on them.
	// Reading one twice would open two recordings for one stretch of speech.
	var l alwaysOn
	l.pendingSpeech = true
	if !l.takePendingSpeech() {
		t.Fatal("takePendingSpeech did not see the flag")
	}
	if l.takePendingSpeech() {
		t.Fatal("takePendingSpeech returned the same flag twice")
	}

	l.pendingFarEnd = true
	if !l.takePendingFarEnd() {
		t.Fatal("takePendingFarEnd did not see the flag")
	}
	if l.takePendingFarEnd() {
		t.Fatal("takePendingFarEnd returned the same flag twice")
	}
}

// The capture callback runs on miniaudio's realtime thread. It must never
// block, and it must survive there being no worker to hand audio to (the
// window between the recorder being stopped and the callback draining).
func TestTheCaptureCallbackNeverBlocks(t *testing.T) {
	a := &app{}
	a.onListenChunk(make([]float32, 1000)) // no worker at all
	if len(a.listen.preroll) != 1000 {
		t.Fatalf("pre-roll = %d samples, want the chunk buffered for a session to start from", len(a.listen.preroll))
	}

	// A worker that has stopped consuming must cost the audio thread
	// nothing: the queue fills, and everything after that is dropped.
	a.listen.work = make(chan listenMsg, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			a.onListenChunk(make([]float32, 160))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the capture callback blocked on a full worker queue")
	}
	if !a.listen.droppedOnce {
		t.Fatal("dropping audio from the gate must be reported, once")
	}
}

// Losing the far-end gate mid-flight (the tap stopped while a chunk was in
// the queue) must not take the worker down with it.
func TestFarEndChunksWithoutAGateAreIgnored(t *testing.T) {
	a := &app{}
	work := make(chan listenMsg, 4)
	done := make(chan struct{})
	gate := &vad.Gate{} // never fed: msgMic is not sent below
	go a.listenWorker(gate, work, done)
	work <- listenMsg{kind: msgFar, samples: make([]float32, 160)}
	work <- listenMsg{kind: msgFarGateClose}
	close(work)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not exit when its queue was closed")
	}
}

func TestPrerollKeepsOnlyItsTail(t *testing.T) {
	// The buffer runs all day. Keeping the tail by reslicing would hold the
	// whole backing array alive, so the copy is the point of the helper.
	max := 4
	buf := appendBounded(nil, []float32{1, 2, 3}, max)
	buf = appendBounded(buf, []float32{4, 5, 6}, max)
	if len(buf) != max {
		t.Fatalf("len = %d, want %d", len(buf), max)
	}
	if buf[0] != 3 || buf[3] != 6 {
		t.Fatalf("kept the wrong end: %v", buf)
	}
	if cap(buf) > max {
		t.Fatalf("cap = %d, want the tail copied into a %d-sample buffer", cap(buf), max)
	}
}

func TestAutoRecordingsWithoutATranscriptAreSwept(t *testing.T) {
	dir := t.TempDir()
	store := settings.NewStore(filepath.Join(dir, "settings.json"))
	cfg := store.Get()
	cfg.AlwaysOn = true
	cfg.AlwaysOnRetentionHours = 1
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}

	meetings := history.NewMeetingStore(dir)
	old := time.Now().Add(-3 * time.Hour)
	recent := time.Now().Add(-10 * time.Minute)

	write := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("not really audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	audioPath, keptPath, manualPath := write("old.wav"), write("kept.wav"), write("manual.wav")

	for _, m := range []history.Meeting{
		{Start: old, AudioPath: audioPath, AutoStarted: true},
		{Start: recent, AudioPath: keptPath, AutoStarted: true},
		{Start: old.Add(-time.Minute), AudioPath: manualPath},
	} {
		if err := meetings.Append(m); err != nil {
			t.Fatal(err)
		}
	}

	a := &app{store: store, meetings: meetings}
	a.sweepAutoRecordings()

	all, err := meetings.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		switch {
		case m.Start.Equal(old):
			if m.AudioPath != "" {
				t.Error("an old auto recording with no transcript kept its audio")
			}
		case m.Start.Equal(recent):
			if m.AudioPath == "" {
				t.Error("a recent auto recording was swept too early")
			}
		default:
			if m.AudioPath == "" {
				t.Error("a recording the user started by hand was swept")
			}
		}
	}
}

// unitVec builds a fingerprint pointing mostly along one axis, so two of
// them are as similar or as different as the test needs.
func unitVec(dim, axis int, bleed float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = bleed
	}
	v[axis] = 1
	return voiceid.Normalize(v)
}

func newTestSession(t *testing.T) *session {
	t.Helper()
	s, err := newSession(t.TempDir(), time.Now(), asr.ModelSpec{}, "auto", true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOneVoiceStaysANote(t *testing.T) {
	s := newTestSession(t)
	me := unitVec(64, 0, 0.01)
	for i := 0; i < 8; i++ {
		if s.noteVoice(me, 4) {
			t.Fatal("one voice talking to itself became a conversation")
		}
	}
	if s.currentKind() != sessionNote {
		t.Fatalf("kind = %q, want %q", s.currentKind(), sessionNote)
	}
}

func TestASecondVoiceMakesItAConversation(t *testing.T) {
	s := newTestSession(t)
	me, them := unitVec(64, 0, 0.01), unitVec(64, 7, 0.01)

	s.noteVoice(me, 5)
	// One reply from someone else is not a conversation: a passer-by, a
	// phrase from a video, a cough that fingerprinted oddly.
	if s.noteVoice(them, 1) {
		t.Fatal("a single short reply escalated")
	}
	if s.currentKind() != sessionNote {
		t.Fatal("kind changed on one reply")
	}
	// Two replies and enough seconds of them is somebody talking.
	if !s.noteVoice(them, 3) {
		t.Fatal("a second voice that cleared the bar did not escalate")
	}
	if s.currentKind() != sessionMeeting {
		t.Fatalf("kind = %q, want %q", s.currentKind(), sessionMeeting)
	}
}

func TestEscalationIsOneWay(t *testing.T) {
	// A conversation does not become a note again because the other person
	// went quiet -- the recording already contains them.
	s := newTestSession(t)
	me, them := unitVec(64, 0, 0.01), unitVec(64, 7, 0.01)
	s.noteVoice(me, 5)
	s.noteVoice(them, 2)
	s.noteVoice(them, 3)
	for i := 0; i < 5; i++ {
		s.noteVoice(me, 4)
	}
	if s.currentKind() != sessionMeeting {
		t.Fatal("a conversation reverted to a note")
	}
}

func TestFarEndSpeechMakesItAConversation(t *testing.T) {
	s := newTestSession(t)
	if s.noteFarEnd(4) {
		t.Fatal("four seconds of far-end audio escalated too early")
	}
	if !s.noteFarEnd(farEndEscalateSeconds) {
		t.Fatal("sustained far-end speech did not escalate")
	}
	if s.currentKind() != sessionMeeting {
		t.Fatalf("kind = %q, want %q", s.currentKind(), sessionMeeting)
	}
}

func TestVoicesWithoutAnExtractorNeverEscalateOnTheirOwn(t *testing.T) {
	// No voice model on disk means no fingerprints, so the session cannot
	// count people. It must stay a note rather than guess.
	s := newTestSession(t)
	for i := 0; i < 10; i++ {
		if s.noteVoice(nil, 5) {
			t.Fatal("escalated with no fingerprints to go on")
		}
	}
	if s.currentKind() != sessionNote {
		t.Fatal("kind changed with no fingerprints")
	}
}

func TestSilenceThresholdsDependOnWhatIsBeingRecorded(t *testing.T) {
	// A note is one thought and ends on a short pause; a conversation has
	// pauses in it and ends on a long one. Mixing these up either chops
	// meetings into pieces or glues remarks together.
	var s settings.Settings
	s.AlwaysOnNoteGapSeconds = 0
	s.AlwaysOnSplitMinutes = 0
	if noteGapSeconds(s) != 60 {
		t.Fatalf("note gap default = %v, want 60s", noteGapSeconds(s))
	}
	if splitMinutes(s) != 5 {
		t.Fatalf("meeting gap default = %v, want 5m", splitMinutes(s))
	}
	s.AlwaysOnNoteGapSeconds = 25
	s.AlwaysOnSplitMinutes = 10
	if noteGapSeconds(s) != 25 || splitMinutes(s) != 10 {
		t.Fatal("configured gaps are not honoured")
	}
}

func TestLoudNoiseDoesNotKeepASessionAlive(t *testing.T) {
	// The bug this replaces: the silence timer ran on audio.Level, so a fan
	// or a keyboard held a session open for minutes and produced a long
	// recording with nothing said in it. Only speech the gate confirmed may
	// touch the timer.
	s := newTestSession(t)
	s.mu.Lock()
	s.lastVoiced = time.Now().Add(-5 * time.Minute)
	s.mu.Unlock()

	loud := make([]float32, 1600)
	for i := range loud {
		loud[i] = 0.9
	}
	s.writeMic(loud)
	s.writeSystem(loud, 1.0)

	if s.quietFor() < 4*time.Minute {
		t.Fatalf("loud non-speech reset the silence timer: quiet for %v", s.quietFor())
	}
	// Far-end *speech* does count -- that is the gate's verdict, not a level.
	s.noteFarEnd(1)
	if s.quietFor() > time.Second {
		t.Fatalf("confirmed far-end speech did not reset the timer: quiet for %v", s.quietFor())
	}
}

func TestConfirmedSpeechKeepsASessionAlive(t *testing.T) {
	s := newTestSession(t)
	s.mu.Lock()
	s.lastVoiced = time.Now().Add(-5 * time.Minute)
	s.mu.Unlock()
	s.noteVoice(unitVec(64, 0, 0.01), 3)
	if s.quietFor() > time.Second {
		t.Fatalf("a confirmed reply did not reset the timer: quiet for %v", s.quietFor())
	}
}

func TestSystemLevelStillCountsTowardsASecondParty(t *testing.T) {
	// sysVoiced answers a different question from the timer -- "did anything
	// at all come out of the far end" -- and still runs on level.
	s := newTestSession(t)
	loud := make([]float32, audio.SampleRate)
	for i := range loud {
		loud[i] = 0.9
	}
	s.writeSystem(loud, 1.0)
	s.mu.Lock()
	got := s.sysVoiced
	s.mu.Unlock()
	if got < 0.9 {
		t.Fatalf("sysVoiced = %v, want about a second", got)
	}
}

// writeSilentWAV lays down a real recording of n seconds, so the orphan
// sweep's own audio.OpenWAV can read it.
func writeSilentWAV(t *testing.T, path string, seconds float64) {
	t.Helper()
	w, err := audio.NewWAVWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(make([]float32, int(seconds*audio.SampleRate))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanSweepLeavesNoteAudioAlone(t *testing.T) {
	// A note whose transcript was still queued when the app quit used to be
	// adopted as a meeting on the next launch -- the recording of a thought
	// said to nobody turned up in Meetings with a speaker bar.
	dir := t.TempDir()
	hist := history.NewStore(dir)
	meetings := history.NewMeetingStore(filepath.Join(dir, "Meetings"))

	recordings := filepath.Join(dir, meetingsDirName)
	if err := os.MkdirAll(recordings, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	notePath := filepath.Join(recordings, at.Format("2006-01-02-150405")+"-mic.wav")
	writeSilentWAV(t, notePath, 5)

	if err := hist.Append(history.Entry{
		Timestamp:        at,
		RecordingSeconds: 5,
		AudioPath:        notePath,
		AutoStarted:      true,
	}); err != nil {
		t.Fatal(err)
	}

	a := &app{hist: hist, meetings: meetings}
	a.adoptOrphanedMeetings()

	all, err := meetings.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("a note was adopted as a meeting: %+v", all)
	}
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("the note's audio was removed: %v", err)
	}
}

func TestOrphanSweepStillAdoptsAnUnknownRecording(t *testing.T) {
	// The other half of the same rule: audio nothing knows about is still a
	// recording that survived a crash, and it must not be lost.
	dir := t.TempDir()
	hist := history.NewStore(dir)
	meetings := history.NewMeetingStore(filepath.Join(dir, "Meetings"))

	recordings := filepath.Join(dir, meetingsDirName)
	if err := os.MkdirAll(recordings, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeSilentWAV(t, filepath.Join(recordings, at.Format("2006-01-02-150405")+"-mic.wav"), 5)

	a := &app{hist: hist, meetings: meetings}
	a.adoptOrphanedMeetings()

	all, err := meetings.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d meetings, want the orphan adopted", len(all))
	}
}
