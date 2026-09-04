package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/history"
	"voxlog-go/internal/settings"
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

// A listener with no gate is the state between "the setting was turned on"
// and "the model finished downloading". Audio still arrives; nothing may be
// kept, and nothing may crash.
func TestChunksWithoutAGateAreDropped(t *testing.T) {
	a := &app{}
	a.onListenChunk(make([]float32, 1000))
	if len(a.listen.preroll) != 0 {
		t.Fatalf("kept %d samples with no gate open", len(a.listen.preroll))
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
