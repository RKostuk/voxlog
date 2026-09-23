package history

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAppendKeepsEveryConcurrentEntry(t *testing.T) {
	// Takes decode independently now, so two transcripts can finish at the
	// same moment. Unguarded, both would read the same day file and write it
	// back with only their own entry added -- one transcript silently gone.
	s := NewStore(t.TempDir())
	day := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Append(Entry{Timestamp: day.Add(time.Duration(i) * time.Second), Text: "take"}); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	entries, err := s.EntriesForDay(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 20 {
		t.Fatalf("got %d entries, want 20 -- concurrent appends lost transcripts", len(entries))
	}
}

func TestUpdateAttachesATranscriptLater(t *testing.T) {
	s := NewStore(t.TempDir())
	at := time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)
	if err := s.Append(Entry{Timestamp: at, AudioPath: "/tmp/d.wav", RecordingSeconds: 4, AutoStarted: true}); err != nil {
		t.Fatal(err)
	}

	if err := s.Update(at, func(e *Entry) { e.Text = "the transcript" }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	entries, err := s.EntriesForDay(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Text != "the transcript" {
		t.Fatalf("got %+v, want the transcript attached", entries)
	}
	if entries[0].AudioPath != "/tmp/d.wav" || entries[0].RecordingSeconds != 4 || !entries[0].AutoStarted {
		t.Fatalf("got %+v, want the entry's other fields preserved", entries[0])
	}
}

func TestUpdateUnknownEntryIsAnError(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Update(time.Now(), func(*Entry) {}); err == nil {
		t.Fatal("Update silently did nothing for an entry that is not there")
	}
}

func TestOldDayFileReadsAsDictation(t *testing.T) {
	// A day file written before meetings existed has no "kind" field. Imported,
	// it must not suddenly become a meeting, or the History window would offer
	// to transcribe audio that was never kept.
	dir := t.TempDir()
	raw := `[{"timestamp":"2026-08-01T09:00:00Z","duration_seconds":1.5,"text":"hello","recording_seconds":3}]`
	if err := os.WriteFile(filepath.Join(dir, "2026-08-01.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(dir)
	if _, err := MigrateDictations(s, filepath.Join(t.TempDir(), "migrated")); err != nil {
		t.Fatal(err)
	}
	entries, err := s.EntriesForDay(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Kind == KindMeeting {
		t.Fatal("an entry with no kind read as a meeting")
	}
	if entries[0].Text != "hello" || entries[0].AudioPath != "" {
		t.Fatalf("got %+v, want the original fields intact", entries[0])
	}
}

func TestDictationEntriesCarryNoMeetingFields(t *testing.T) {
	// A dictation never had a meeting's fields, and moving into a table it
	// shares with nothing must not invent them: an audio path that appeared out
	// of nowhere is a Delete audio button for a file that does not exist.
	dir := t.TempDir()
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	s := NewStore(dir)
	if err := s.Append(Entry{Timestamp: at, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.EntriesForDay(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Kind != "" || got.AudioPath != "" || got.SystemAudioPath != "" || got.AutoStarted {
		t.Fatalf("got %+v, want a bare dictation", got)
	}
	if !got.Timestamp.Equal(at) {
		t.Fatalf("timestamp came back as %v, want %v", got.Timestamp, at)
	}
}
