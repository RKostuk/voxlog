package main

import (
	"testing"

	"voxlog-go/internal/diarize"
)

// A meeting is diarized whole and decoded a block at a time, so every block
// has to be handed the part of that answer it covers, in its own clock.
func TestBlockSegmentsCutsToTheBlockAndRebasesItsClock(t *testing.T) {
	all := []diarize.Segment{
		{Start: 10, End: 20, Speaker: 0},
		{Start: 55, End: 65, Speaker: 1}, // straddles the 60s boundary
		{Start: 70, End: 80, Speaker: 0},
	}

	first := blockSegments(all, 0, 60)
	want := []diarize.Segment{
		{Start: 10, End: 20, Speaker: 0},
		{Start: 55, End: 60, Speaker: 1},
	}
	if len(first) != len(want) {
		t.Fatalf("first block: got %v, want %v", first, want)
	}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("first block segment %d: got %v, want %v", i, first[i], want[i])
		}
	}

	second := blockSegments(all, 60, 120)
	want = []diarize.Segment{
		{Start: 0, End: 5, Speaker: 1},   // the other half, from the block's start
		{Start: 10, End: 20, Speaker: 0}, // 70s-80s, ten seconds into the block
	}
	if len(second) != len(want) {
		t.Fatalf("second block: got %v, want %v", second, want)
	}
	for i := range want {
		if second[i] != want[i] {
			t.Fatalf("second block segment %d: got %v, want %v", i, second[i], want[i])
		}
	}

	// The speaker numbers are the meeting's, not the block's: the same person
	// is 1 in both halves of the turn they were cut out of.
	if first[1].Speaker != second[0].Speaker {
		t.Fatalf("a turn split across blocks changed speaker: %v %v", first[1], second[0])
	}
}
