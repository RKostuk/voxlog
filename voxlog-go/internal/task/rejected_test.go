package task

import (
	"fmt"
	"testing"
)

func TestAppendRejectedBoundsToMax(t *testing.T) {
	s := NewStore(t.TempDir())
	for i := 0; i < maxRejected+3; i++ {
		if err := s.AppendRejected(fmt.Sprintf("example %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.LoadRejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != maxRejected {
		t.Fatalf("got %d entries, want capped at %d", len(list), maxRejected)
	}
}

func TestLoadRejectedMissingFileIsNotAnError(t *testing.T) {
	s := NewStore(t.TempDir())
	list, err := s.LoadRejected()
	if err != nil || list != nil {
		t.Fatalf("got (%v, %v), want (nil, nil) for no file yet", list, err)
	}
}

func TestRemoveRejectedDropsOnlyTheNamedEntry(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, text := range []string{"first", "second", "third"} {
		if err := s.AppendRejected(text); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveRejected("second"); err != nil {
		t.Fatal(err)
	}
	list, err := s.LoadRejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0] != "first" || list[1] != "third" {
		t.Fatalf("got %v, want [first third]", list)
	}
}

func TestRemoveRejectedUnknownEntryIsANoOp(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.AppendRejected("only"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRejected("never added"); err != nil {
		t.Fatalf("removing an absent entry should not fail: %v", err)
	}
	list, err := s.LoadRejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != "only" {
		t.Fatalf("got %v, want [only]", list)
	}
}

func TestGetAndDeleteRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	tk := Task{ID: NewID(), Text: "send report"}
	if err := s.Append(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(tk.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.Delete(tk.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(tk.ID); err == nil {
		t.Fatal("Get after Delete: want an error, task should be gone")
	}
	// Deleting again must not error -- gone is gone.
	if err := s.Delete(tk.ID); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

// The same line rejected twice is one example, moved to the end -- it used to
// be stored twice, which spent two of the prompt's eight slots saying the same
// thing.
func TestRejectingTheSameLineTwiceKeepsOneCopy(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, text := range []string{"buy milk", "call back", "buy milk"} {
		if err := s.AppendRejected(text); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.LoadRejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %v, want two distinct examples", list)
	}
	if list[len(list)-1] != "buy milk" {
		t.Fatalf("got %v, want the re-rejected line last", list)
	}
}
