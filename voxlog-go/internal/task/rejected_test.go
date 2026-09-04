package task

import "testing"

func TestAppendRejectedBoundsToMax(t *testing.T) {
	s := NewStore(t.TempDir())
	for i := 0; i < maxRejected+3; i++ {
		if err := s.AppendRejected("example"); err != nil {
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
