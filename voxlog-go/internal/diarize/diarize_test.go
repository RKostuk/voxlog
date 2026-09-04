package diarize

import "testing"

func TestMergeJoinsSameSpeakerAcrossShortGaps(t *testing.T) {
	got := Merge([]Segment{
		{Start: 0, End: 2, Speaker: 0},
		{Start: 2.4, End: 4, Speaker: 0}, // a breath, not a new turn
		{Start: 4.2, End: 6, Speaker: 1},
	}, 1.0, 0.5)

	if len(got) != 2 {
		t.Fatalf("got %+v, want 2 turns", got)
	}
	if got[0].End != 4 || got[0].Speaker != 0 {
		t.Errorf("first turn is %+v, want speaker 0 running to 4s", got[0])
	}
}

func TestMergeKeepsSameSpeakerAcrossALongGap(t *testing.T) {
	got := Merge([]Segment{
		{Start: 0, End: 2, Speaker: 0},
		{Start: 30, End: 32, Speaker: 0}, // someone else held the floor between
	}, 1.0, 0.5)
	if len(got) != 2 {
		t.Fatalf("got %+v, want the two stretches kept apart", got)
	}
}

func TestMergeDropsSlivers(t *testing.T) {
	// Every surviving segment costs one pass through the ASR model, and a
	// 200ms sliver decodes to junk.
	got := Merge([]Segment{
		{Start: 0, End: 0.2, Speaker: 0},
		{Start: 5, End: 8, Speaker: 1},
	}, 1.0, 0.5)
	if len(got) != 1 || got[0].Speaker != 1 {
		t.Fatalf("got %+v, want only the long segment", got)
	}
}

func TestSliceClampsToTheBuffer(t *testing.T) {
	samples := make([]float32, 16000) // one second
	// The segmentation model runs on its own frame grid and can report an end
	// past the last sample; slicing that raw would panic.
	got := Slice(samples, Segment{Start: 0.5, End: 1.5}, 16000)
	if len(got) != 8000 {
		t.Fatalf("got %d samples, want 8000", len(got))
	}
	if got := Slice(samples, Segment{Start: 2, End: 3}, 16000); got != nil {
		t.Fatalf("a segment past the end should slice to nothing, got %d samples", len(got))
	}
}
