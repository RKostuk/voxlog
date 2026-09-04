package task

import (
	"testing"
	"time"
)

func TestAppendUpdateAllRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())

	t1 := Task{ID: NewID(), SourceKind: "dictation", SourceKey: "k1", Text: "send report", Entity: "Acme", Status: StatusTodo, Created: time.Now()}
	if err := s.Append(t1); err != nil {
		t.Fatal(err)
	}

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Text != "send report" {
		t.Fatalf("got %+v", all)
	}

	if err := s.Update(t1.ID, func(t *Task) { t.Status = StatusDone }); err != nil {
		t.Fatal(err)
	}
	all, _ = s.All()
	if all[0].Status != StatusDone {
		t.Fatalf("got status %q, want done", all[0].Status)
	}
}

func TestEntityNamesDedupsCaseInsensitively(t *testing.T) {
	s := NewStore(t.TempDir())
	for i, entity := range []string{"Acme", "acme", "Beta"} {
		id := NewID() + string(rune('a'+i))
		if err := s.Append(Task{ID: id, Entity: entity, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := s.EntityNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("got %v, want 2 distinct entities", names)
	}
}

func TestUpdateUnknownTaskErrors(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Update("does-not-exist", func(t *Task) {}); err == nil {
		t.Fatal("want an error updating a task that was never appended")
	}
}
