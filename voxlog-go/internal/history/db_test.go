package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedMeeting(t *testing.T, s *MeetingStore, at time.Time) {
	t.Helper()
	if err := s.Append(Meeting{Start: at, RecordingSeconds: 300, AudioPath: "/tmp/m-mic.wav"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceTurnsRoundTrips(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, at)

	speakers := []MeetingSpeaker{
		{LocalID: YouSpeaker, TalkSecs: 12, TurnCount: 2},
		{LocalID: 1, TalkSecs: 8, TurnCount: 1, Embed: []float32{1, 0, 0}},
	}
	turns := []Turn{
		{Channel: ChannelMic, StartSecs: 1, EndSecs: 5, LocalID: YouSpeaker, Text: "first"},
		{Channel: ChannelSystem, StartSecs: 5, EndSecs: 13, LocalID: 1, Text: "second"},
		{Channel: ChannelMic, StartSecs: 13, EndSecs: 21, LocalID: YouSpeaker, Text: "third"},
	}
	if err := s.ReplaceTurns(at, speakers, turns, 1); err != nil {
		t.Fatalf("ReplaceTurns: %v", err)
	}

	got, err := s.Turns(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d turns, want 3", len(got))
	}
	if got[1].Text != "second" || got[1].LocalID != 1 || got[1].Channel != ChannelSystem {
		t.Fatalf("second turn came back wrong: %+v", got[1])
	}
	// Absolute times are the entire reason turns are stored; a turn that
	// forgot when it happened cannot be played back.
	if got[2].StartSecs != 13 || got[2].EndSecs != 21 {
		t.Fatalf("turn timings not preserved: %+v", got[2])
	}

	sp, err := s.Speakers(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp) != 2 || sp[0].LocalID != YouSpeaker || sp[1].TalkSecs != 8 {
		t.Fatalf("speakers came back wrong: %+v", sp)
	}
	if len(sp[1].Embed) != 3 || sp[1].Embed[0] != 1 {
		t.Fatalf("embedding did not survive the round trip: %+v", sp[1].Embed)
	}

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if all[0].TurnsVersion != 1 {
		t.Fatalf("meeting was not stamped with the turn version: %+v", all[0])
	}
}

// The background backfill re-decodes old meetings and can be interrupted at
// any point, so it will run over the same meeting more than once. Twice must
// look exactly like once.
func TestReplaceTurnsIsIdempotent(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, at)

	speakers := []MeetingSpeaker{{LocalID: 1, TalkSecs: 4, TurnCount: 1}}
	turns := []Turn{{StartSecs: 0, EndSecs: 4, LocalID: 1, Text: "hello"}}

	for i := 0; i < 3; i++ {
		if err := s.ReplaceTurns(at, speakers, turns, 1); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	got, _ := s.Turns(at)
	if len(got) != 1 {
		t.Fatalf("got %d turns after three runs, want 1", len(got))
	}
	sp, _ := s.Speakers(at)
	if len(sp) != 1 {
		t.Fatalf("got %d speakers after three runs, want 1", len(sp))
	}
}

func TestReplaceTurnsForAnUnknownMeetingIsAnError(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	if err := s.ReplaceTurns(time.Now(), nil, nil, 1); err == nil {
		t.Fatal("turns were written for a meeting that does not exist")
	}
}

func TestSpeakersByMeetingGroupsThem(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	second := first.Add(time.Hour)
	seedMeeting(t, s, first)
	seedMeeting(t, s, second)

	if err := s.ReplaceTurns(first, []MeetingSpeaker{{LocalID: 1, TalkSecs: 10}}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTurns(second, []MeetingSpeaker{{LocalID: 1, TalkSecs: 20}, {LocalID: 2, TalkSecs: 5}}, nil, 1); err != nil {
		t.Fatal(err)
	}

	byMeeting, err := s.SpeakersByMeeting()
	if err != nil {
		t.Fatal(err)
	}
	if len(byMeeting[first.UnixNano()]) != 1 || len(byMeeting[second.UnixNano()]) != 2 {
		t.Fatalf("speakers were not grouped by meeting: %+v", byMeeting)
	}
}

func TestSetEntityFilesAMeeting(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, at)

	if err := s.SetEntity(at, "Voxlog"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.All()
	if all[0].Entity != "Voxlog" {
		t.Fatalf("got entity %q, want Voxlog", all[0].Entity)
	}
	if err := s.SetEntity(at.Add(time.Hour), "Voxlog"); err == nil {
		t.Fatal("filing a meeting that does not exist silently succeeded")
	}
}

// The old records are the user's history; the import has to bring all of it
// across, survive being re-run, and never lose a transcript to a record that
// has none.
func TestMigrateJSONMeetingsToDB(t *testing.T) {
	dir := t.TempDir()
	jsonDir := filepath.Join(dir, "Meetings")
	if err := os.MkdirAll(jsonDir, 0o755); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 8, 1, 9, 30, 0, 0, time.Local)
	write := func(name string, m Meeting) {
		t.Helper()
		raw, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(filepath.Join(jsonDir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", Meeting{Start: at, RecordingSeconds: 120, Text: "the transcript", AudioPath: "/tmp/a.wav"})
	write("b.json", Meeting{Start: at.Add(time.Hour), RecordingSeconds: 60})
	write("broken.json", Meeting{})
	if err := os.WriteFile(filepath.Join(jsonDir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewMeetingStore(dir)
	sentinel := filepath.Join(dir, "migrated-sqlite")
	n, err := MigrateJSONMeetingsToDB(s, jsonDir, sentinel)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d meetings, want 2 (the unreadable one is skipped, not fatal)", n)
	}

	got, _ := s.All()
	if len(got) != 2 || got[1].Text != "the transcript" {
		t.Fatalf("migrated meetings came back wrong: %+v", got)
	}

	// The JSON is a backup now, not a deleted intermediate.
	if _, err := os.Stat(filepath.Join(jsonDir, "a.json")); err != nil {
		t.Fatalf("migration deleted the JSON backup: %v", err)
	}

	// Second run is a no-op thanks to the sentinel, and even without it the
	// insert would conflict rather than duplicate.
	if n, err := MigrateJSONMeetingsToDB(s, jsonDir, sentinel); err != nil || n != 0 {
		t.Fatalf("second run imported %d meetings (err %v), want 0", n, err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateJSONMeetingsToDB(s, jsonDir, sentinel); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.All(); len(got) != 2 {
		t.Fatalf("re-running the import duplicated meetings: %d rows", len(got))
	}
}

// A transcript won since the import must not be overwritten by the stale copy
// still sitting in the JSON backup.
func TestMigrateDoesNotClobberANewerTranscript(t *testing.T) {
	dir := t.TempDir()
	jsonDir := filepath.Join(dir, "Meetings")
	if err := os.MkdirAll(jsonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 1, 9, 30, 0, 0, time.Local)
	raw, _ := json.Marshal(Meeting{Start: at, RecordingSeconds: 120})
	if err := os.WriteFile(filepath.Join(jsonDir, "a.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewMeetingStore(dir)
	if err := s.Append(Meeting{Start: at, RecordingSeconds: 120, Text: "decoded after the move"}); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateJSONMeetingsToDB(s, jsonDir, filepath.Join(dir, "migrated-sqlite")); err != nil {
		t.Fatal(err)
	}

	got, _ := s.All()
	if got[0].Text != "decoded after the move" {
		t.Fatalf("import overwrote a newer transcript with an empty one: %+v", got[0])
	}
}

func TestVecRoundTrip(t *testing.T) {
	in := []float32{0.5, -0.25, 1}
	out := DecodeVec(EncodeVec(in))
	if len(out) != 3 || out[0] != 0.5 || out[1] != -0.25 || out[2] != 1 {
		t.Fatalf("got %v, want %v", out, in)
	}
	if EncodeVec(nil) != nil {
		t.Fatal("an empty vector must encode as NULL, not as an empty blob")
	}
	if DecodeVec([]byte{1, 2, 3}) != nil {
		t.Fatal("a blob that cannot be an embedding must decode as nothing")
	}
}
