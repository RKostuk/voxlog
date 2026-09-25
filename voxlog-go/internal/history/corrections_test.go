package history

import (
	"testing"
	"time"
)

// A meeting whose replies are all far-end speech, cut where the caller says.
func meetingWithTurns(t *testing.T, s *MeetingStore, at time.Time, speakers []MeetingSpeaker, turns []Turn, version int) {
	t.Helper()
	if err := s.Append(Meeting{Start: at, RecordingSeconds: 600, AudioPath: "/tmp/mic.wav", SystemAudioPath: "/tmp/sys.wav"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTurns(at, speakers, turns, version); err != nil {
		t.Fatal(err)
	}
}

func turnAt(seq int, start, end float64, local int) Turn {
	return Turn{Seq: seq, Channel: ChannelSystem, StartSecs: start, EndSecs: end, LocalID: local, Text: "said something"}
}

func speakerNamed(t *testing.T, s *MeetingStore, at time.Time, local int, name string) Voice {
	t.Helper()
	speakers, err := s.Speakers(at)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range speakers {
		if sp.LocalID == local {
			v, err := s.NameSpeaker(sp.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("no speaker %d in the meeting", local)
	return Voice{}
}

func turnNames(t *testing.T, s *MeetingStore, at time.Time) []string {
	t.Helper()
	turns, err := s.Turns(at)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(turns))
	for i, turn := range turns {
		out[i] = turn.Name
	}
	return out
}

func TestCorrectingAReplyMovesItToTheNamedVoice(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 24, 23, 2, 0, 0, time.Local)
	meetingWithTurns(t, s, at,
		[]MeetingSpeaker{
			{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
			{LocalID: 1, TalkSecs: 10, TurnCount: 1, Embed: fakeVoice(2)},
		},
		[]Turn{
			turnAt(0, 0, 10, 0),
			turnAt(1, 10, 20, 1),
			turnAt(2, 20, 30, 0), // really the second speaker
		}, 1)

	ildar := speakerNamed(t, s, at, 1, "Ільдар")

	if _, err := s.CorrectTurn(at, 2, ildar.ID); err != nil {
		t.Fatalf("CorrectTurn: %v", err)
	}
	if got := turnNames(t, s, at)[2]; got != "Ільдар" {
		t.Fatalf("the corrected reply reads as %q, want Ільдар", got)
	}
}

// The reason corrections are not stored on the turn. Raising the extraction
// version is how the backfill re-runs an improved pipeline over old meetings,
// and it rewrites every turn and speaker row -- so a correction that lived on
// one would be destroyed by exactly the improvement it was meant to outlast.
func TestACorrectionSurvivesTheMeetingBeingDecodedAgain(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 24, 23, 2, 0, 0, time.Local)
	meetingWithTurns(t, s, at,
		[]MeetingSpeaker{
			{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
			{LocalID: 1, TalkSecs: 10, TurnCount: 1, Embed: fakeVoice(2)},
		},
		[]Turn{turnAt(0, 0, 10, 0), turnAt(1, 10, 20, 1), turnAt(2, 20, 30, 0)}, 1)

	ildar := speakerNamed(t, s, at, 1, "Ільдар")
	if _, err := s.CorrectTurn(at, 2, ildar.ID); err != nil {
		t.Fatalf("CorrectTurn: %v", err)
	}

	// A better pipeline, cutting the same speech into different replies: the
	// corrected stretch is now two turns, and every id is new.
	if err := s.ReplaceTurns(at, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
	}, []Turn{
		turnAt(0, 0, 9.5, 0),
		turnAt(1, 10.2, 19.8, 0),
		turnAt(2, 20.4, 25, 0),
		turnAt(3, 25.2, 29.6, 0),
	}, 2); err != nil {
		t.Fatalf("ReplaceTurns: %v", err)
	}

	names := turnNames(t, s, at)
	if names[2] != "Ільдар" || names[3] != "Ільдар" {
		t.Fatalf("the correction did not survive the re-decode: %v", names)
	}
	if names[0] == "Ільдар" || names[1] == "Ільдар" {
		t.Fatalf("the correction spread past the stretch it covered: %v", names)
	}
}

// A reply belongs to the correction its middle falls inside. Overlap alone
// would let a correction claim the neighbour it merely touches.
func TestACorrectionClaimsTheReplyItCoversAndNotItsNeighbour(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 24, 23, 2, 0, 0, time.Local)
	meetingWithTurns(t, s, at,
		[]MeetingSpeaker{
			{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
			{LocalID: 1, TalkSecs: 10, TurnCount: 1, Embed: fakeVoice(2)},
		},
		[]Turn{
			turnAt(0, 40.0, 44.2, 0), // ends just inside the correction
			turnAt(1, 44.1, 47.0, 1), // the one being corrected
			turnAt(2, 47.3, 52.2, 0),
		}, 1)

	voice := speakerNamed(t, s, at, 0, "Кочевников")
	if _, err := s.CorrectTurn(at, 1, voice.ID); err != nil {
		t.Fatalf("CorrectTurn: %v", err)
	}

	// Re-decode with the same boundaries: the correction must still pick out
	// only the reply it was made on.
	if err := s.ReplaceTurns(at, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
		{LocalID: 1, TalkSecs: 10, TurnCount: 1, Embed: fakeVoice(2)},
	}, []Turn{
		turnAt(0, 40.0, 44.2, 0),
		turnAt(1, 44.1, 47.0, 1),
		turnAt(2, 47.3, 52.2, 0),
	}, 2); err != nil {
		t.Fatalf("ReplaceTurns: %v", err)
	}

	turns, err := s.Turns(at)
	if err != nil {
		t.Fatal(err)
	}
	if turns[1].Name != "Кочевников" {
		t.Fatalf("the corrected reply reads as %q", turns[1].Name)
	}
	if turns[0].SpeakerID == turns[1].SpeakerID {
		t.Fatalf("the correction also claimed the reply that merely overlaps it")
	}
}

// Merging is the common repair: the pipeline split one person in two.
func TestMergingSpeakersMovesEveryReplyAndOutlivesADecode(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 24, 23, 2, 0, 0, time.Local)
	meetingWithTurns(t, s, at,
		[]MeetingSpeaker{
			{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
			{LocalID: 1, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0.2)},
		},
		[]Turn{
			turnAt(0, 0, 10, 0),
			turnAt(1, 10, 20, 1),
			turnAt(2, 20, 30, 0),
			turnAt(3, 30, 40, 1),
		}, 1)

	speakerNamed(t, s, at, 0, "Ільдар")
	speakers, err := s.Speakers(at)
	if err != nil {
		t.Fatal(err)
	}
	var from, into int64
	for _, sp := range speakers {
		if sp.LocalID == 0 {
			into = sp.ID
		} else {
			from = sp.ID
		}
	}

	if err := s.MergeSpeakers(from, into); err != nil {
		t.Fatalf("MergeSpeakers: %v", err)
	}
	for i, name := range turnNames(t, s, at) {
		if name != "Ільдар" {
			t.Fatalf("reply %d reads as %q after the merge", i, name)
		}
	}

	if err := s.ReplaceTurns(at, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
		{LocalID: 1, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0.2)},
	}, []Turn{
		turnAt(0, 0, 10, 0), turnAt(1, 10, 20, 1),
		turnAt(2, 20, 30, 0), turnAt(3, 30, 40, 1),
	}, 2); err != nil {
		t.Fatalf("ReplaceTurns: %v", err)
	}
	for i, name := range turnNames(t, s, at) {
		if name != "Ільдар" {
			t.Fatalf("reply %d reads as %q after the re-decode", i, name)
		}
	}
}

func TestClearingACorrectionReturnsTheReplyToThePipeline(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 24, 23, 2, 0, 0, time.Local)
	meetingWithTurns(t, s, at,
		[]MeetingSpeaker{
			{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
			{LocalID: 1, TalkSecs: 10, TurnCount: 1, Embed: fakeVoice(2)},
		},
		[]Turn{turnAt(0, 0, 10, 0), turnAt(1, 10, 20, 1), turnAt(2, 20, 30, 0)}, 1)

	ildar := speakerNamed(t, s, at, 1, "Ільдар")
	if _, err := s.CorrectTurn(at, 2, ildar.ID); err != nil {
		t.Fatalf("CorrectTurn: %v", err)
	}
	if err := s.ClearCorrection(at, 2); err != nil {
		t.Fatalf("ClearCorrection: %v", err)
	}

	turns, err := s.Turns(at)
	if err != nil {
		t.Fatal(err)
	}
	if turns[2].SpeakerID != turns[0].SpeakerID {
		t.Fatalf("the reply did not go back to the speaker the pipeline gave it")
	}
}

// A correction whose stretch no longer holds any speech is kept, not dropped:
// the next pipeline may well find a reply there again.
func TestACorrectionWithNothingLeftToApplyToIsKept(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 24, 23, 2, 0, 0, time.Local)
	meetingWithTurns(t, s, at,
		[]MeetingSpeaker{
			{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
			{LocalID: 1, TalkSecs: 10, TurnCount: 1, Embed: fakeVoice(2)},
		},
		[]Turn{turnAt(0, 0, 10, 0), turnAt(1, 10, 20, 1), turnAt(2, 20, 30, 0)}, 1)

	ildar := speakerNamed(t, s, at, 1, "Ільдар")
	if _, err := s.CorrectTurn(at, 2, ildar.ID); err != nil {
		t.Fatalf("CorrectTurn: %v", err)
	}

	// A decode that finds nothing in the corrected stretch.
	if err := s.ReplaceTurns(at, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
	}, []Turn{turnAt(0, 0, 10, 0), turnAt(1, 10, 20, 0)}, 2); err != nil {
		t.Fatalf("ReplaceTurns: %v", err)
	}

	// And one that finds it again.
	if err := s.ReplaceTurns(at, []MeetingSpeaker{
		{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
	}, []Turn{turnAt(0, 0, 10, 0), turnAt(1, 10, 20, 0), turnAt(2, 20, 30, 0)}, 3); err != nil {
		t.Fatalf("ReplaceTurns: %v", err)
	}
	if got := turnNames(t, s, at)[2]; got != "Ільдар" {
		t.Fatalf("the correction was forgotten while it had nothing to apply to: %q", got)
	}
}

// Identification must never overwrite what a person decided.
func TestIdentifyLeavesACorrectedSpeakerAlone(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	earlier := time.Date(2026, 9, 20, 10, 0, 0, 0, time.Local)
	meetingWithTurns(t, s, earlier,
		[]MeetingSpeaker{{LocalID: 0, TalkSecs: 300, TurnCount: 9, Embed: fakeVoice(0)}},
		[]Turn{turnAt(0, 0, 300, 0)}, 1)
	speakerNamed(t, s, earlier, 0, "Кочевников")

	at := time.Date(2026, 9, 24, 23, 2, 0, 0, time.Local)
	meetingWithTurns(t, s, at,
		[]MeetingSpeaker{
			{LocalID: 0, TalkSecs: 20, TurnCount: 2, Embed: fakeVoice(0)},
			{LocalID: 1, TalkSecs: 10, TurnCount: 1, Embed: fakeVoice(2)},
		},
		[]Turn{turnAt(0, 0, 20, 0), turnAt(1, 20, 30, 1)}, 1)

	ildar := speakerNamed(t, s, at, 1, "Ільдар")
	// The first speaker sounds exactly like Кочевников, but the person who was
	// there says otherwise.
	if _, err := s.CorrectTurn(at, 0, ildar.ID); err != nil {
		t.Fatalf("CorrectTurn: %v", err)
	}

	if _, err := s.IdentifySpeakers(at); err != nil {
		t.Fatalf("IdentifySpeakers: %v", err)
	}
	if got := turnNames(t, s, at)[0]; got != "Ільдар" {
		t.Fatalf("identification overwrote a correction: %q", got)
	}
}
