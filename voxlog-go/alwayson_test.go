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
	"voxlog-go/internal/voiceprint"
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
	if a.listen.dropped == 0 {
		t.Fatal("dropping audio from the gate must be counted, so the session it cost can report it")
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
	return voiceprint.Normalize(v)
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
	// A couple of replies from someone else is not a conversation: a
	// passer-by, a phrase from a video, a cough that fingerprinted oddly.
	if s.noteVoice(them, 1) {
		t.Fatal("a single short reply escalated")
	}
	if s.noteVoice(them, 2) {
		t.Fatal("three seconds over two replies escalated")
	}
	if s.currentKind() != sessionNote {
		t.Fatal("kind changed before the bar was cleared")
	}
	// Three replies and eight seconds of them is somebody talking.
	if !s.noteVoice(them, 6) {
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
	s.noteVoice(them, 3)
	s.noteVoice(them, 3)
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

func TestOnePersonAtTwoDistancesIsStillOnePerson(t *testing.T) {
	// The failure that made the feature look broken: one person talking to
	// themselves escalated to a conversation, and the note then waited for
	// the five-minute conversation gap instead of the one-minute note gap.
	// A cluster far enough from the first to stand on its own (below
	// voiceprint.MergeThreshold) can still be plainly the same voice.
	s := newTestSession(t)
	me := unitVec(64, 0, 0)
	// Cosine 0.5 to me: its own cluster, but nothing like a second person.
	nearlyMe := make([]float32, 64)
	nearlyMe[0], nearlyMe[1] = 0.5, 0.866
	nearlyMe = voiceprint.Normalize(nearlyMe)

	s.noteVoice(me, 10)
	for i := 0; i < 6; i++ {
		if s.noteVoice(nearlyMe, 5) {
			t.Fatal("the same voice at a different distance opened a conversation")
		}
	}
	if s.currentKind() != sessionNote {
		t.Fatalf("kind = %q, want it to still be a note", s.currentKind())
	}
	if len(s.voices) != 2 {
		t.Fatalf("got %d clusters, want the two the merge threshold gives", len(s.voices))
	}
}

func TestAnUnconfirmedRecordingGetsSecondsNotAMinute(t *testing.T) {
	// A recording opens on the gate's first impression, so the menu bar can
	// say "recording" while the first sentence is still being said. If the
	// second stage never agrees, that guess must cost seconds -- not the
	// minute a real note is given to finish a thought.
	var cfg settings.Settings
	cfg.AlwaysOnNoteGapSeconds = 60

	s := newTestSession(t)
	if s.isConfirmed() {
		t.Fatal("a session starts unconfirmed")
	}
	s.mu.Lock()
	s.lastVoiced = time.Now().Add(-20 * time.Second)
	s.mu.Unlock()

	// Unconfirmed: 20 seconds of nothing is already past the grace period.
	if s.quietFor() < provisionalGrace {
		t.Fatal("the test's own clock is wrong")
	}

	// Confirmed: the same 20 seconds is well inside the note gap, and the
	// recording keeps running.
	s.noteVoice(unitVec(64, 0, 0.01), 3)
	if !s.isConfirmed() {
		t.Fatal("a confirmed reply did not mark the session")
	}
	if s.quietFor() > time.Duration(noteGapSeconds(cfg)*float64(time.Second)) {
		t.Fatal("a confirmed session should still be well within its gap")
	}
}

func TestFarEndSpeechAlsoConfirms(t *testing.T) {
	s := newTestSession(t)
	s.noteFarEnd(1)
	if !s.isConfirmed() {
		t.Fatal("far-end speech must confirm the recording too")
	}
}

func TestYieldingTheMicIsIdempotentAndReversible(t *testing.T) {
	// A dictation takes the microphone at once rather than on the
	// supervisor's next tick, because two seconds of the gate still
	// listening is two seconds of it reacting to the words being dictated.
	a := &app{}
	if a.listen.isYielded() {
		t.Fatal("the microphone starts with always-on")
	}
	a.yieldMicToUser()
	if !a.listen.isYielded() {
		t.Fatal("yielding did not stick")
	}
	// Twice in a row is what a double press looks like, and must not wait on
	// a worker that is already gone.
	a.yieldMicToUser()

	a.reclaimMic()
	if a.listen.isYielded() {
		t.Fatal("the microphone was not given back")
	}
}

func TestAYieldedMicOpensNoSession(t *testing.T) {
	// The worker decides this on its own, without asking the app whether a
	// dictation is running: that question takes a.mu, which the hotkey path
	// holds while waiting for the worker to drain.
	a := &app{}
	a.listen.listening = true
	a.listen.yielded = true
	if sess := a.openSessionIfIdle(false); sess != nil {
		t.Fatal("a session opened while the user had the microphone")
	}
}

// -- the mode-wide microphone mute (§12) ------------------------------------

// Muting must not shorten the microphone track. The mic and the far end are
// decoded against one clock, so a file missing the muted seconds would drag
// every word after them ahead of the other side of the call.
func TestMutingWritesSilenceRatherThanNothing(t *testing.T) {
	a := &app{}
	loud := make([]float32, 480)
	for i := range loud {
		loud[i] = 0.5
	}

	a.onListenChunk(loud)
	if len(a.listen.preroll) != len(loud) {
		t.Fatalf("pre-roll = %d samples, want %d", len(a.listen.preroll), len(loud))
	}

	a.toggleListenMute()
	a.onListenChunk(loud)
	if got, want := len(a.listen.preroll), 2*len(loud); got != want {
		t.Fatalf("pre-roll = %d samples, want %d -- a mute must keep the clock, not skip it", got, want)
	}
	for _, v := range a.listen.preroll[len(loud):] {
		if v != 0 {
			t.Fatal("the muted stretch of the pre-roll is not silent")
		}
	}
	for _, v := range a.listen.preroll[:len(loud)] {
		if v != 0.5 {
			t.Fatal("muting rewrote audio recorded before the mute")
		}
	}
}

// While muted the gate hears nothing, so the user's own voice can neither
// hold a session open nor open a new one.
func TestAMutedMicrophoneNeverReachesTheGate(t *testing.T) {
	a := &app{}
	a.listen.work = make(chan listenMsg, 8)

	a.onListenChunk(make([]float32, 160))
	if len(a.listen.work) != 1 {
		t.Fatalf("unmuted: worker got %d messages, want 1", len(a.listen.work))
	}

	a.toggleListenMute()
	a.onListenChunk(make([]float32, 160))
	if len(a.listen.work) != 1 {
		t.Fatalf("muted: worker got %d messages, want the microphone withheld", len(a.listen.work))
	}

	a.toggleListenMute()
	a.onListenChunk(make([]float32, 160))
	if len(a.listen.work) != 2 {
		t.Fatalf("unmuted again: worker got %d messages, want 2", len(a.listen.work))
	}
}

// Unlike the meeting's mute, this one is a state of the mode: it has to
// survive a session opening and closing under it.
func TestTheListenMuteIsAModeStateNotARecordingState(t *testing.T) {
	a := &app{}
	if a.micMuted() {
		t.Fatal("always-on starts unmuted")
	}
	if !a.toggleListenMute() || !a.micMuted() {
		t.Fatal("the first click must mute")
	}

	// A session comes and goes; the mute does not.
	a.listen.mu.Lock()
	a.listen.sess = &session{}
	a.listen.mu.Unlock()
	if !a.micMuted() {
		t.Fatal("opening a session cleared the mode-wide mute")
	}
	a.listen.mu.Lock()
	a.listen.sess = nil
	a.listen.mu.Unlock()
	if !a.micMuted() {
		t.Fatal("closing a session cleared the mode-wide mute")
	}

	if a.toggleListenMute() || a.micMuted() {
		t.Fatal("the second click must unmute")
	}
}

// The far end is not muted: a call you sit quietly through is exactly what
// the tap is for.
func TestMutingLeavesTheFarEndAlone(t *testing.T) {
	a := &app{}
	a.listen.work = make(chan listenMsg, 4)
	a.toggleListenMute()

	loud := make([]float32, 160)
	for i := range loud {
		loud[i] = 0.4
	}
	a.onFarEndChunk(loud)
	if len(a.listen.work) != 1 {
		t.Fatal("the far end must still reach the gate while the microphone is muted")
	}
	if len(a.listen.farPreroll) != len(loud) || a.listen.farPreroll[0] == 0 {
		t.Fatal("the far-end pre-roll must keep the audio it heard")
	}
}

// Two mutes, one glyph: the meeting's dies with its recording, always-on's
// does not, and the menu bar draws either.
func TestTheMenuBarShowsEitherMute(t *testing.T) {
	tr := &tray{}
	tr.listenMuted = true
	tr.mu.Lock()
	got := trayStateFor(tr.recording, tr.decoding, tr.meeting, tr.anyMuteLocked())
	tr.mu.Unlock()
	if got != stateMuted {
		t.Fatalf("state = %q, want %q for a mode-wide mute", got, stateMuted)
	}

	// Starting a recording clears the meeting's mute and keeps the mode's.
	tr.micMuted = true
	tr.startedMeeting(time.Now())
	if tr.micMuted {
		t.Fatal("a new recording must start unmuted")
	}
	if !tr.listenMuted {
		t.Fatal("always-on's mute must survive a session opening under it")
	}
}

// The voice extractor used to be handed a whole reply, and a reply has no
// maximum length -- so a long monologue meant a long model call on the
// worker, and everything the microphone delivered meanwhile was dropped.
func TestTheEmbeddingOnlySeesItsLoudestFewSeconds(t *testing.T) {
	long := make([]float32, int(30*audio.SampleRate))
	// One loud burst well into the recording; everything else is quiet.
	at := int(20 * audio.SampleRate)
	for i := at; i < at+int(audio.SampleRate); i++ {
		long[i] = 0.8
	}

	got := loudestStretch(long, embedSeconds)
	if want := int(embedSeconds * audio.SampleRate); len(got) != want {
		t.Fatalf("got %d samples, want %d", len(got), want)
	}
	var peak float32
	for _, v := range got {
		if v > peak {
			peak = v
		}
	}
	if peak < 0.5 {
		t.Fatalf("the stretch kept has peak %.2f -- it missed the loud part entirely", peak)
	}
}

// Anything already short enough is handed over untouched: copying it would
// cost a second allocation per reply for nothing.
func TestAShortReplyIsEmbeddedWhole(t *testing.T) {
	short := make([]float32, int(audio.SampleRate))
	if got := loudestStretch(short, embedSeconds); len(got) != len(short) {
		t.Fatalf("got %d samples, want the whole %d", len(got), len(short))
	}
}

// The silence timer belongs to the gate, not to the confirmed replies. A
// quiet speaker never clears autoSpeechLevel, and when that was the only
// thing moving lastVoiced their recording was closed mid-sentence.
func TestTheGateKeepsTheSilenceTimerAlive(t *testing.T) {
	sess := &session{lastVoiced: time.Now().Add(-time.Minute)}
	if sess.quietFor() < 30*time.Second {
		t.Fatal("the fixture is wrong: the session should look long quiet")
	}

	sess.heardVoice()
	if quiet := sess.quietFor(); quiet > time.Second {
		t.Fatalf("the gate reported speech and the session is still %v quiet", quiet)
	}
	// It moves the timer and nothing else: deciding who is talking still
	// belongs to the second stage.
	if sess.isConfirmed() {
		t.Error("the gate alone must not mark a recording as confirmed speech")
	}
}

// What makes a recording worth keeping is that somebody spoke in it. It used
// to be that the second stage recognised a voice -- which has a loudness
// floor under it, so a quiet sentence meant the file, the row and everything
// said were deleted.
func TestASessionIsKeptForTheSpeechInIt(t *testing.T) {
	sess := &session{}
	if sess.voicedSeconds() != 0 {
		t.Fatal("a fresh session claims speech nobody has heard")
	}

	sess.addVoiced(0.6)
	if sess.voicedSeconds() >= minVoicedToKeep {
		t.Fatalf("%.1fs of speech should not be enough to keep a recording", sess.voicedSeconds())
	}
	sess.addVoiced(0.6)
	if sess.voicedSeconds() < minVoicedToKeep {
		t.Fatalf("%.1fs of speech should be enough to keep a recording", sess.voicedSeconds())
	}
	// Never confirmed, and that no longer decides anything about keeping it.
	if sess.isConfirmed() {
		t.Error("counting speech must not stand in for recognising a voice")
	}
}

// Speech also moves the silence timer: the two go together, and a recording
// the gate is still hearing must not be closed underneath the speaker.
func TestCountingSpeechKeepsTheRecordingOpen(t *testing.T) {
	sess := &session{lastVoiced: time.Now().Add(-time.Minute)}
	sess.addVoiced(1)
	if quiet := sess.quietFor(); quiet > time.Second {
		t.Fatalf("the session is %v quiet right after speech was counted in it", quiet)
	}
}

// A recording whose Close never ran -- the app was killed mid-sentence --
// used to claim an empty data chunk and read back as nothing at all. The
// length is now refreshed as it goes, so what survives is the audio.
func TestARecordingThatWasNeverClosedStillReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "killed.wav")
	w, err := audio.NewWAVWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	// Thirty seconds, written in chunks, and then no Close at all.
	chunk := make([]float32, 1600)
	for i := 0; i < 300; i++ {
		if err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}

	if got := audio.DurationSeconds(path); got < 20 {
		t.Fatalf("an unclosed 30s recording reads back as %.1fs", got)
	}
}

// And a file with nothing in it is still nothing, which is what keeps an
// empty row out of History.
func TestAnEmptyRecordingHasNoDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.wav")
	w, err := audio.NewWAVWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := audio.DurationSeconds(path); got != 0 {
		t.Fatalf("an empty recording reads back as %.1fs", got)
	}
	if got := audio.DurationSeconds(filepath.Join(t.TempDir(), "nope.wav")); got != 0 {
		t.Fatalf("a missing recording reads back as %.1fs", got)
	}
}

// Handing the microphone back has to be acted on now, not on the
// supervisor's own schedule: the two seconds after a dictation ends are
// exactly when the user carries on talking.
func TestGivingTheMicrophoneBackWakesTheSupervisor(t *testing.T) {
	a := &app{}
	a.listen.wake = make(chan struct{}, 1)
	a.listen.yielded = true

	a.reclaimMic()
	if a.listen.isYielded() {
		t.Fatal("the microphone was not handed back")
	}
	select {
	case <-a.listen.wake:
	default:
		t.Fatal("the supervisor was left to find out on its next tick")
	}
}

// A nudge with nobody listening for it must not block, and two of them are
// the same request.
func TestNudgingIsSafeAndIdempotent(t *testing.T) {
	var l alwaysOn
	l.nudge() // no channel yet: startAlwaysOn has not run

	l.wake = make(chan struct{}, 1)
	l.nudge()
	l.nudge()
	if len(l.wake) != 1 {
		t.Fatalf("two nudges queued %d wake-ups, want one", len(l.wake))
	}
}
