package history

import (
	"strings"
	"testing"
	"time"
)

func TestSearchTurnsFindsAcrossMeetings(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	second := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, first)
	seedMeeting(t, s, second)

	speakers := []MeetingSpeaker{{LocalID: YouSpeaker, TalkSecs: 10, TurnCount: 1}}
	if err := s.ReplaceTurns(first, speakers, []Turn{
		{Channel: ChannelMic, StartSecs: 1, EndSecs: 5, LocalID: YouSpeaker, Text: "we should send the invoice on Friday"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTurns(second, speakers, []Turn{
		{Channel: ChannelMic, StartSecs: 2, EndSecs: 9, LocalID: YouSpeaker, Text: "the invoice was paid"},
		{Channel: ChannelMic, StartSecs: 9, EndSecs: 12, LocalID: YouSpeaker, Text: "nothing to do with money"},
	}, 1); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchTurns("invoice")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want the two turns mentioning the invoice: %+v", len(hits), hits)
	}
	// Newest meeting first: search is a way back into a recent conversation
	// far more often than into an old one.
	if !hits[0].Start.Equal(second) {
		t.Fatalf("first hit is from %v, want the newer meeting %v", hits[0].Start, second)
	}
	if !strings.Contains(hits[0].Snippet, "[[") {
		t.Fatalf("snippet does not mark the match: %q", hits[0].Snippet)
	}
}

func TestSearchTurnsMatchesPartialWords(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, at)
	if err := s.ReplaceTurns(at,
		[]MeetingSpeaker{{LocalID: YouSpeaker, TalkSecs: 4, TurnCount: 1}},
		[]Turn{{Channel: ChannelMic, StartSecs: 0, EndSecs: 4, LocalID: YouSpeaker, Text: "the invoice is late"}},
		1); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchTurns("invoi")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits for a prefix, want 1", len(hits))
	}
}

func TestSearchTurnsSurvivesWhateverIsTyped(t *testing.T) {
	// The caller is a search box that queries on every keystroke, so a
	// half-typed FTS operator must come back empty rather than as an error.
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, at)
	if err := s.ReplaceTurns(at,
		[]MeetingSpeaker{{LocalID: YouSpeaker, TalkSecs: 4, TurnCount: 1}},
		[]Turn{{Channel: ChannelMic, StartSecs: 0, EndSecs: 4, LocalID: YouSpeaker, Text: "plain words only"}},
		1); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "   ", `"`, "-", "NEAR(", "*", "AND OR", `plain" OR "x`} {
		if _, err := s.SearchTurns(q); err != nil {
			t.Fatalf("SearchTurns(%q): %v", q, err)
		}
	}
}

func TestSearchIndexFollowsTheTurnsItIndexes(t *testing.T) {
	// The FTS table is external-content: a re-decode replaces every turn of
	// a meeting, and the index has to lose the old text with them or search
	// would keep finding transcripts that no longer exist.
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, at)
	speakers := []MeetingSpeaker{{LocalID: YouSpeaker, TalkSecs: 4, TurnCount: 1}}

	if err := s.ReplaceTurns(at, speakers,
		[]Turn{{Channel: ChannelMic, StartSecs: 0, EndSecs: 4, LocalID: YouSpeaker, Text: "misheard nonsense"}}, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTurns(at, speakers,
		[]Turn{{Channel: ChannelMic, StartSecs: 0, EndSecs: 4, LocalID: YouSpeaker, Text: "corrected wording"}}, 2); err != nil {
		t.Fatal(err)
	}

	stale, err := s.SearchTurns("misheard")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("search still finds replaced text: %+v", stale)
	}
	fresh, err := s.SearchTurns("corrected")
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatalf("got %d hits for the new text, want 1", len(fresh))
	}
}

func TestPeopleAggregatesAcrossMeetings(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	second := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, first)
	seedMeeting(t, s, second)

	for _, at := range []time.Time{first, second} {
		if err := s.ReplaceTurns(at, []MeetingSpeaker{
			{LocalID: YouSpeaker, TalkSecs: 30, TurnCount: 3},
			{LocalID: 1, TalkSecs: 20, TurnCount: 2, Embed: []float32{1, 0, 0}},
		}, []Turn{
			{Channel: ChannelMic, StartSecs: 0, EndSecs: 30, LocalID: YouSpeaker, Text: "mine"},
			{Channel: ChannelSystem, StartSecs: 30, EndSecs: 50, LocalID: 1, Text: "theirs"},
		}, 1); err != nil {
			t.Fatal(err)
		}
	}

	// Only named voices are counted, so nothing shows up until the speaker
	// in each meeting is recognised as the same person.
	people, err := s.People()
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 0 {
		t.Fatalf("unnamed speakers should not appear as people: %+v", people)
	}

	sp, err := s.Speakers(first)
	if err != nil {
		t.Fatal(err)
	}
	var voice Voice
	for _, one := range sp {
		if one.LocalID == 1 {
			voice, err = s.NameSpeaker(one.ID, "Olena")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	sp2, err := s.Speakers(second)
	if err != nil {
		t.Fatal(err)
	}
	for _, one := range sp2 {
		if one.LocalID == 1 {
			if err := s.LinkSpeaker(one.ID, voice.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	people, err = s.People()
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 {
		t.Fatalf("got %d people, want 1: %+v", len(people), people)
	}
	got := people[0]
	if got.Name != "Olena" || got.Meetings != 2 || got.TalkSecs != 40 || got.TurnCount != 4 {
		t.Fatalf("aggregate is wrong: %+v", got)
	}
	if !got.LastSeen.Equal(second) {
		t.Fatalf("LastSeen = %v, want the later meeting %v", got.LastSeen, second)
	}
}
