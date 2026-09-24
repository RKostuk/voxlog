package history

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMeetingRoundTrips(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 8, 18, 11, 0, 0, 0, time.Local)
	want := Meeting{
		Start:            at,
		RecordingSeconds: 2531,
		AudioPath:        "/tmp/m-mic.wav",
		SystemAudioPath:  "/tmp/m-system.wav",
	}
	if err := s.Append(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d meetings, want 1", len(got))
	}
	if !got[0].Start.Equal(want.Start) || got[0].RecordingSeconds != want.RecordingSeconds ||
		got[0].AudioPath != want.AudioPath || got[0].SystemAudioPath != want.SystemAudioPath {
		t.Fatalf("got %+v, want %+v", got[0], want)
	}
}

// A transcript arrives minutes after the call ended, so the record has to be
// reopened and added to without disturbing what the recording left behind.
func TestUpdateAttachesTheTranscriptLater(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 8, 18, 11, 0, 0, 0, time.Local)
	if err := s.Append(Meeting{Start: at, RecordingSeconds: 60, AudioPath: "/tmp/m.wav"}); err != nil {
		t.Fatal(err)
	}

	if err := s.Update(at, func(m *Meeting) {
		m.Text = "the transcript"
		m.DurationSeconds = 4.5
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.All()
	if len(got) != 1 || got[0].Text != "the transcript" || got[0].DurationSeconds != 4.5 {
		t.Fatalf("got %+v, want the transcript attached", got)
	}
	if got[0].AudioPath != "/tmp/m.wav" || got[0].RecordingSeconds != 60 {
		t.Fatalf("got %+v, want the recording's own fields preserved", got[0])
	}
}

func TestUpdateUnknownMeetingIsAnError(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	if err := s.Update(time.Now(), func(*Meeting) {}); err == nil {
		t.Fatal("Update silently did nothing for a meeting that is not there")
	}
}

// Distinct meetings write to distinct files, so this is not a lock test --
// filepath.Glob and os.MkdirAll are safe to call concurrently on their own.
// It stays because a directory that fills up with real meetings one at a
// time is the normal case, and it is cheap insurance that nothing about
// concurrent unrelated writes trips the glob in All.
func TestConcurrentAppendsToDifferentMeetingsKeepThemAll(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	day := time.Date(2026, 8, 18, 9, 0, 0, 0, time.Local)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Append(Meeting{Start: day.Add(time.Duration(i) * time.Minute), RecordingSeconds: 10}); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("got %d meetings, want 20", len(got))
	}
}

// A transcript update can land while the same meeting's record is being
// (re-)written by something else -- an Append re-run and an Update racing
// on one Start. This is what mu actually guards: unlocked, one goroutine's
// os.Rename can land between another's read and write of that one file, so
// the record on disk ends up with fields stitched together from two
// different writers, or briefly missing altogether mid-swap.
//
// Every writer here sets a pair of fields that only make sense together, so
// whichever writer's result survives, its own pair must still agree with
// itself -- proof that no interleaving tore the record apart.
func TestConcurrentAppendAndUpdateOnTheSameMeetingStayCoherent(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 8, 18, 11, 0, 0, 0, time.Local)
	if err := s.Append(Meeting{Start: at, RecordingSeconds: 60, AudioPath: "/tmp/m.wav"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A full record, replacing whatever is there. Its two fields
			// are derived from the same i, so they must land together.
			err := s.Append(Meeting{
				Start:            at,
				RecordingSeconds: float64(i),
				AudioPath:        fmt.Sprintf("/tmp/append-%d.wav", i),
			})
			if err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Can legitimately race the very first Append and find nothing
			// there yet -- that is a real "no meeting" error, not a bug.
			_ = s.Update(at, func(m *Meeting) {
				m.Text = fmt.Sprintf("take %d", i)
				m.DurationSeconds = float64(i)
			})
		}(i)
	}
	wg.Wait()

	got, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d meetings, want 1 -- concurrent writers must not fork the record into two files", len(got))
	}
	m := got[0]

	// RecordingSeconds and AudioPath are always written together by a single
	// Append call -- the seed one or one of the racing ones -- and Update
	// never touches either. Whichever pair survived, it must be one Append
	// actually wrote; a mismatch means two different Appends got spliced
	// together by an unlocked interleaving. (An Update landing after the
	// last Append is a legitimate, fully serialized reordering -- not a
	// tear -- so this does not assume the seed record survived.)
	switch m.AudioPath {
	case "/tmp/m.wav":
		if m.RecordingSeconds != 60 {
			t.Fatalf("torn recording fields, mixed with the seed record: %+v", m)
		}
	default:
		var i int
		if _, err := fmt.Sscanf(m.AudioPath, "/tmp/append-%d.wav", &i); err != nil {
			t.Fatalf("torn audio path field, does not parse as one writer's value: %+v", m)
		}
		if m.RecordingSeconds != float64(i) {
			t.Fatalf("torn recording fields, mixed two different Append calls: %+v", m)
		}
	}

	// Text and DurationSeconds are always written together by a single
	// Update call. Whichever one survived (if any), its pair must agree.
	if m.Text != "" {
		var i int
		if _, err := fmt.Sscanf(m.Text, "take %d", &i); err != nil {
			t.Fatalf("torn text field, does not parse as one writer's value: %+v", m)
		}
		if m.DurationSeconds != float64(i) {
			t.Fatalf("torn transcript fields, mixed two different Update calls: %+v", m)
		}
	}
}

func TestAllReturnsNewestFirst(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.Local)
	for _, offset := range []time.Duration{0, 2 * time.Hour, time.Hour} {
		if err := s.Append(Meeting{Start: base.Add(offset), RecordingSeconds: 10}); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := s.All()
	for i := 1; i < len(got); i++ {
		if got[i-1].Start.Before(got[i].Start) {
			t.Fatalf("meeting %d is older than the one after it: %v", i-1, got)
		}
	}
}

// A directory that does not exist yet is the normal state on first run, and a
// stray file in it is not a reason to lose every real meeting beside it.
func TestAllToleratesAMissingDirectoryAndStrayFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewMeetingStore(filepath.Join(dir, "not-there-yet"))
	if got, err := s.All(); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want an empty list and no error", got, err)
	}

	s = NewMeetingStore(dir)
	if err := s.Append(Meeting{Start: time.Now(), RecordingSeconds: 10}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d meetings, want the one real meeting kept", len(got))
	}
}

func TestGetReturnsTheStoredMeeting(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, at)

	m, err := s.Get(at)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Start.Equal(at) {
		t.Fatalf("got a meeting starting %v, want %v", m.Start, at)
	}
	if m.RecordingSeconds != 300 {
		t.Fatalf("got %v recording seconds, want the seeded 300", m.RecordingSeconds)
	}
}

func TestGetOnAMissingMeetingErrors(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)

	if _, err := s.Get(at); err == nil {
		t.Fatal("reading a meeting that was never recorded returned no error")
	}
}

// A project the user typed is the answer; a project summarization picked is a
// guess. The guess must never come back over the top of the answer -- not
// even a later one, since the transcript can be re-summarized at any time.
func TestAutoEntityNeverOverwritesAHandSetProject(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	start := time.Date(2026, 9, 23, 9, 0, 0, 0, time.Local)
	seedMeeting(t, s, start)

	set, err := s.SetEntityAuto(start, "Northwind")
	if err != nil {
		t.Fatal(err)
	}
	if !set {
		t.Fatal("a meeting nobody has filed by hand refused the automatic project")
	}
	if m, _ := s.Get(start); m.Entity != "Northwind" {
		t.Fatalf("entity = %q, want Northwind", m.Entity)
	}

	if err := s.SetEntity(start, "Contoso"); err != nil {
		t.Fatal(err)
	}
	set, err = s.SetEntityAuto(start, "Northwind")
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Error("the automatic project overwrote one set by hand")
	}
	if m, _ := s.Get(start); m.Entity != "Contoso" {
		t.Fatalf("entity = %q, want the hand-set Contoso", m.Entity)
	}
}

// "No project" chosen by hand is a choice, and an empty guess is not a reason
// to clear a project an earlier pass got right.
func TestAutoEntityLeavesWhatItCannotImproveOn(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	start := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)
	seedMeeting(t, s, start)

	if _, err := s.SetEntityAuto(start, "Northwind"); err != nil {
		t.Fatal(err)
	}
	if set, err := s.SetEntityAuto(start, ""); err != nil || set {
		t.Fatalf("an empty guess reported set=%v err=%v, want false/nil", set, err)
	}
	if m, _ := s.Get(start); m.Entity != "Northwind" {
		t.Fatalf("entity = %q, want Northwind left alone", m.Entity)
	}

	if err := s.SetEntity(start, ""); err != nil {
		t.Fatal(err)
	}
	if set, _ := s.SetEntityAuto(start, "Contoso"); set {
		t.Error("a project cleared by hand was refilled automatically")
	}
	if m, _ := s.Get(start); m.Entity != "" {
		t.Fatalf("entity = %q, want the hand-cleared empty", m.Entity)
	}
}

// Summarization is the only thing that names a meeting, and a model that
// came back with nothing must not take away a name an earlier pass produced.
func TestSetTitleAutoNamesAMeetingAndRefusesAnEmptyName(t *testing.T) {
	s := NewMeetingStore(t.TempDir())
	at := time.Date(2026, 9, 22, 10, 4, 0, 0, time.Local)
	if err := s.Append(Meeting{Start: at, RecordingSeconds: 2748}); err != nil {
		t.Fatal(err)
	}

	// A meeting recorded before titles existed shows its date, not a name.
	if m, err := s.Get(at); err != nil || m.Title != "" {
		t.Fatalf("Get = %q, %v; want an untitled meeting", m.Title, err)
	}
	if err := s.SetTitleAuto(at, "CSV importer scope"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Get(at); m.Title != "CSV importer scope" {
		t.Errorf("title = %q", m.Title)
	}
	if err := s.SetTitleAuto(at, ""); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Get(at); m.Title != "CSV importer scope" {
		t.Errorf("an empty answer cleared the title: %q", m.Title)
	}
	if err := s.SetTitleAuto(at.Add(time.Hour), "nobody"); err == nil {
		t.Error("naming a meeting that does not exist reported success")
	}
}
