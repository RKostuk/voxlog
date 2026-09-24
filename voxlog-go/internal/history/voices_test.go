package history

import (
	"math"
	"testing"
	"time"
)

// A stand-in voice: a direction in space. Two angles far apart are two
// different people; the same angle with a nudge is the same person on a
// different day.
func fakeVoice(angle float64) []float32 {
	v := []float32{float32(math.Cos(angle)), float32(math.Sin(angle)), 0.1}
	return centroid([][]float32{v}, []float64{1})
}

func meetingWithSpeakers(t *testing.T, s *MeetingStore, at time.Time, speakers []MeetingSpeaker) []MeetingSpeaker {
	t.Helper()
	if err := s.Append(Meeting{Start: at, RecordingSeconds: 600, AudioPath: "/tmp/mic.wav", SystemAudioPath: "/tmp/sys.wav"}); err != nil {
		t.Fatal(err)
	}
	var turns []Turn
	for i, sp := range speakers {
		turns = append(turns, Turn{
			Channel:   ChannelSystem,
			StartSecs: float64(i * 10),
			EndSecs:   float64(i*10) + sp.TalkSecs,
			LocalID:   sp.LocalID,
			Text:      "said something",
		})
	}
	if err := s.ReplaceTurns(at, speakers, turns, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.Speakers(at)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestNamingASpeakerCreatesAVoiceAndSticksToIt(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	sp := meetingWithSpeakers(t, s, at, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 120, TurnCount: 3, Embed: fakeVoice(0)},
	})

	v, err := s.NameSpeaker(sp[0].ID, "Ірина")
	if err != nil {
		t.Fatalf("NameSpeaker: %v", err)
	}
	if v.Name != "Ірина" || len(v.Embed) == 0 {
		t.Fatalf("voice came back without a name or a fingerprint: %+v", v)
	}
	if v.Secs != 120 {
		t.Fatalf("voice weight is %v, want the speaker's 120 seconds", v.Secs)
	}

	// The name must reach the transcript, which is the whole point.
	turns, err := s.Turns(at)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Name != "Ірина" {
		t.Fatalf("turn still reads as %q", turns[0].Name)
	}
}

// The same name used twice is the same person, not a second voice with a
// colliding label.
func TestNamingTwoSpeakersTheSameNameJoinsOneVoice(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	first := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	second := first.Add(2 * time.Hour)
	a := meetingWithSpeakers(t, s, first, []MeetingSpeaker{{LocalID: 0, TalkSecs: 100, Embed: fakeVoice(0)}})
	b := meetingWithSpeakers(t, s, second, []MeetingSpeaker{{LocalID: 0, TalkSecs: 50, Embed: fakeVoice(0.05)}})

	if _, err := s.NameSpeaker(a[0].ID, "Олег"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NameSpeaker(b[0].ID, "олег"); err != nil { // same name, different case
		t.Fatal(err)
	}

	voices, err := s.Voices()
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 1 {
		t.Fatalf("got %d voices, want 1: %+v", len(voices), voices)
	}
	if voices[0].Secs != 150 {
		t.Fatalf("voice weight is %v, want both speakers' 150 seconds", voices[0].Secs)
	}
}

// Renaming changes the label and nothing else: the same voice keeps matching
// the same people.
func TestRenameKeepsTheIdentity(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	sp := meetingWithSpeakers(t, s, at, []MeetingSpeaker{{LocalID: 0, TalkSecs: 60, Embed: fakeVoice(0)}})
	v, _ := s.NameSpeaker(sp[0].ID, "Speaker from Aster")

	if err := s.RenameVoice(v.ID, "Марта"); err != nil {
		t.Fatal(err)
	}
	voices, _ := s.Voices()
	if len(voices) != 1 || voices[0].Name != "Марта" {
		t.Fatalf("rename did not take: %+v", voices)
	}
	if Cosine(voices[0].Embed, v.Embed) < 0.99 {
		t.Fatal("rename changed the fingerprint; a name is a label, not an identity")
	}
	if err := s.RenameVoice(v.ID+999, "nobody"); err == nil {
		t.Fatal("renaming a voice that does not exist silently succeeded")
	}
}

// Forget is the reversible one: the name goes, the replies stay, and the
// per-meeting fingerprints survive so the voice can be named again.
func TestForgetUnnamesButKeepsTheFingerprints(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	sp := meetingWithSpeakers(t, s, at, []MeetingSpeaker{{LocalID: 0, TalkSecs: 60, Embed: fakeVoice(0)}})
	v, _ := s.NameSpeaker(sp[0].ID, "Олег")

	if err := s.ForgetVoice(v.ID); err != nil {
		t.Fatal(err)
	}

	turns, _ := s.Turns(at)
	if len(turns) != 1 || turns[0].Name != "" {
		t.Fatalf("turns still carry a forgotten name: %+v", turns)
	}
	after, _ := s.Speakers(at)
	if after[0].VoiceID != 0 {
		t.Fatalf("speaker is still linked to a deleted voice: %+v", after[0])
	}
	if len(after[0].Embed) == 0 {
		t.Fatal("Forget destroyed the fingerprint; that is Erase's job, and it must be reversible")
	}
}

// Erase is the other one, and it has to actually destroy what was learned
// about how somebody sounds.
func TestEraseDestroysTheFingerprints(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	sp := meetingWithSpeakers(t, s, at, []MeetingSpeaker{{LocalID: 0, TalkSecs: 60, Embed: fakeVoice(0)}})
	v, _ := s.NameSpeaker(sp[0].ID, "Олег")

	if err := s.EraseVoice(v.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Speakers(at)
	if len(after[0].Embed) != 0 {
		t.Fatal("Erase left the voice fingerprint on disk")
	}
	if voices, _ := s.Voices(); len(voices) != 0 {
		t.Fatalf("Erase left the voice behind: %+v", voices)
	}
}

func TestUnlinkRebuildsTheVoiceWithoutThatSpeaker(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	first := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	second := first.Add(2 * time.Hour)
	a := meetingWithSpeakers(t, s, first, []MeetingSpeaker{{LocalID: 0, TalkSecs: 100, Embed: fakeVoice(0)}})
	b := meetingWithSpeakers(t, s, second, []MeetingSpeaker{{LocalID: 0, TalkSecs: 100, Embed: fakeVoice(1.2)}})

	v, _ := s.NameSpeaker(a[0].ID, "Олег")
	if err := s.LinkSpeaker(b[0].ID, v.ID); err != nil {
		t.Fatal(err)
	}
	// Wrongly linked: the profile now sits between two different voices.
	if err := s.UnlinkSpeaker(b[0].ID); err != nil {
		t.Fatal(err)
	}

	voices, _ := s.Voices()
	if voices[0].Secs != 100 {
		t.Fatalf("voice weight is %v, want the one remaining speaker's 100", voices[0].Secs)
	}
	if Cosine(voices[0].Embed, fakeVoice(0)) < 0.99 {
		t.Fatal("the correction never reached the fingerprint, so the same wrong match will happen again")
	}
}

// Naming one voice should offer the clips that are obviously the same person,
// closest first, and leave the strangers out.
func TestSimilarUnnamedRanksByVoice(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	meetingWithSpeakers(t, s, base, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 30, Embed: fakeVoice(0.02)},
		{LocalID: 1, TalkSecs: 30, Embed: fakeVoice(2.0)},
	})
	meetingWithSpeakers(t, s, base.Add(time.Hour), []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 30, Embed: fakeVoice(0.30)},
	})

	got, err := s.SimilarUnnamed(fakeVoice(0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want the two that sound like this voice: %+v", len(got), got)
	}
	if got[0].Score < got[1].Score {
		t.Fatalf("candidates are not closest-first: %+v", got)
	}
	if got[0].Text == "" || got[0].AudioPath == "" {
		t.Fatalf("a candidate must say where to hear it: %+v", got[0])
	}
	// The far-away voice is not a suggestion at all.
	for _, c := range got {
		if c.Score < SuggestThreshold {
			t.Fatalf("a stranger was offered as a match: %+v", c)
		}
	}
}

// A named voice must be recognised on its own the next time it turns up, and
// a merely-similar one must be asked about rather than assumed.
func TestIdentifySpeakersNamesTheSureOnesAndAsksAboutTheRest(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	known := meetingWithSpeakers(t, s, base, []MeetingSpeaker{{LocalID: 0, TalkSecs: 200, Embed: fakeVoice(0)}})
	if _, err := s.NameSpeaker(known[0].ID, "Олег"); err != nil {
		t.Fatal(err)
	}

	later := base.Add(24 * time.Hour)
	meetingWithSpeakers(t, s, later, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 90, Embed: fakeVoice(0.05)}, // plainly him
		{LocalID: 1, TalkSecs: 90, Embed: fakeVoice(0.85)}, // maybe him, maybe not
		{LocalID: 2, TalkSecs: 90, Embed: fakeVoice(2.6)},  // nobody known
	})

	unsure, err := s.IdentifySpeakers(later)
	if err != nil {
		t.Fatal(err)
	}

	speakers, _ := s.Speakers(later)
	if speakers[0].Name != "Олег" {
		t.Fatalf("the obvious match was not named: %+v", speakers[0])
	}
	if speakers[1].VoiceID != 0 || speakers[2].VoiceID != 0 {
		t.Fatalf("an uncertain match was named without asking: %+v", speakers)
	}
	if len(unsure) != 1 || unsure[0].Name != "Олег" {
		t.Fatalf("got %+v, want one suggestion about Олег", unsure)
	}
}

func TestVoiceClipRoundTrips(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	sp := meetingWithSpeakers(t, s, at, []MeetingSpeaker{{LocalID: 0, TalkSecs: 60, Embed: fakeVoice(0)}})
	v, _ := s.NameSpeaker(sp[0].ID, "Олег")

	if err := s.SetVoiceClip(v.ID, []byte("RIFF....WAVE")); err != nil {
		t.Fatal(err)
	}
	clip, err := s.VoiceClip(v.ID)
	if err != nil || string(clip) != "RIFF....WAVE" {
		t.Fatalf("got %q, %v", clip, err)
	}
	voices, _ := s.Voices()
	if !voices[0].HasClip {
		t.Fatal("the voice does not admit to having a sample")
	}
}

// A voice is the speakers attached to it -- that is what recomputeVoice
// averages -- so the pane has to be able to ask which ones those are.
func TestSpeakersByVoiceReturnsWhatTheVoiceIsMadeOf(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	first := time.Date(2026, 9, 20, 9, 0, 0, 0, time.Local)
	second := time.Date(2026, 9, 21, 9, 0, 0, 0, time.Local)
	for _, at := range []time.Time{first, second} {
		if err := s.Append(Meeting{Start: at, RecordingSeconds: 600}); err != nil {
			t.Fatal(err)
		}
	}
	writeSpeakers := func(at time.Time, secs float64) int64 {
		t.Helper()
		if err := s.ReplaceTurns(at, []MeetingSpeaker{{
			LocalID: 0, TalkSecs: secs, TurnCount: 2, Embed: []float32{1, 0, 0},
		}}, []Turn{{Seq: 0, LocalID: 0, StartSecs: 0, EndSecs: secs, Text: "hello"}}, 1); err != nil {
			t.Fatal(err)
		}
		speakers, err := s.Speakers(at)
		if err != nil || len(speakers) != 1 {
			t.Fatalf("Speakers = %v, %v", speakers, err)
		}
		return speakers[0].ID
	}

	quiet, loud := writeSpeakers(first, 30), writeSpeakers(second, 90)
	v, err := s.NameSpeaker(quiet, "Alex")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkSpeaker(loud, v.ID); err != nil {
		t.Fatal(err)
	}

	clips, err := s.SpeakersByVoice(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 2 {
		t.Fatalf("got %d clips, want both speakers", len(clips))
	}
	// Longest-talking first: that is the clip most worth hearing.
	if clips[0].SpeakerRow != loud {
		t.Errorf("clips are not ordered by talk time: %v", clips)
	}
	if none, err := s.SpeakersByVoice(v.ID + 999); err != nil || len(none) != 0 {
		t.Errorf("SpeakersByVoice on an unknown voice = %v, %v", none, err)
	}
}
