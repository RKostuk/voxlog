package diarize

import (
	"testing"

	"voxlog-go/internal/audio"
)

func TestWindowBoundsCoverAShortRecordingInOnePass(t *testing.T) {
	got := windowBounds(30*audio.SampleRate, WindowSeconds, OverlapSeconds, audio.SampleRate)
	if len(got) != 1 || got[0].from != 0 || got[0].to != 30*audio.SampleRate {
		t.Fatalf("a recording shorter than a window should be one window: %v", got)
	}
}

func TestWindowBoundsOverlapAndReachTheEnd(t *testing.T) {
	const total = 25 * 60 * audio.SampleRate
	got := windowBounds(total, WindowSeconds, OverlapSeconds, audio.SampleRate)
	if len(got) < 2 {
		t.Fatalf("25 minutes should not fit in one 10-minute window: %v", got)
	}
	if got[len(got)-1].to != total {
		t.Fatalf("the last window stops short of the recording: %v", got[len(got)-1])
	}
	for i := 1; i < len(got); i++ {
		if got[i].from >= got[i-1].to {
			t.Fatalf("windows %d and %d do not overlap: %v %v", i-1, i, got[i-1], got[i])
		}
	}
}

// The overlap exists so a speaker change near a seam is heard with context on
// both sides -- but it must be reported once, by one window, not twice.
func TestAcceptRangesTileTheRecordingWithoutGapsOrDoubles(t *testing.T) {
	const total = 25 * 60 * audio.SampleRate
	all := windowBounds(total, WindowSeconds, OverlapSeconds, audio.SampleRate)

	var last float32
	for i, b := range all {
		lo, hi := acceptRange(b, all, audio.SampleRate)
		if lo != last {
			t.Fatalf("window %d starts accepting at %.2f, previous stopped at %.2f", i, lo, last)
		}
		if hi <= lo {
			t.Fatalf("window %d accepts nothing: %.2f to %.2f", i, lo, hi)
		}
		last = hi
	}
	if want := float32(total) / audio.SampleRate; last != want {
		t.Fatalf("the windows together accept %.2fs of a %.2fs recording", last, want)
	}
}

func TestClipTrimsSegmentsThatStraddleTheCut(t *testing.T) {
	in := []Segment{
		{Start: 0, End: 5, Speaker: 0},   // wholly before
		{Start: 8, End: 12, Speaker: 1},  // straddles the start
		{Start: 14, End: 16, Speaker: 2}, // wholly inside
		{Start: 18, End: 25, Speaker: 3}, // straddles the end
		{Start: 30, End: 40, Speaker: 4}, // wholly after
	}
	got := clip(in, 10, 20)
	want := []Segment{
		{Start: 10, End: 12, Speaker: 1},
		{Start: 14, End: 16, Speaker: 2},
		{Start: 18, End: 20, Speaker: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// Whoever speaks first is Speaker 1, whatever the model called them. A gap in
// the model's own numbering must not surface as "Speaker 6" in a call between
// three people.
func TestByFirstAppearanceNumbersInReadingOrder(t *testing.T) {
	got := byFirstAppearance([]Segment{
		{Start: 0, End: 2, Speaker: 4},
		{Start: 2, End: 4, Speaker: 9},
		{Start: 4, End: 6, Speaker: 4},
		{Start: 6, End: 8, Speaker: 0},
	})
	want := []int{0, 1, 0, 2}
	for i, w := range want {
		if got[i].Speaker != w {
			t.Fatalf("segment %d: got speaker %d, want %d (%v)", i, got[i].Speaker, w, got)
		}
	}
}

// A speaker heard only in passing has no steady fingerprint; one heard for
// minutes is described by their longest continuous stretches, trimmed.
func TestFingerprintSegmentsPreferLongContinuousSpeech(t *testing.T) {
	got := pickFingerprintSegments([]Segment{
		{Start: 0, End: 1},     // too short to describe a voice
		{Start: 2, End: 12},    // longest
		{Start: 20, End: 26},   // second
		{Start: 30, End: 34},   // third
		{Start: 40, End: 43.5}, // fourth, over the cap
	})
	if len(got) != maxFingerprintWindows {
		t.Fatalf("got %d stretches, want %d: %v", len(got), maxFingerprintWindows, got)
	}
	for _, s := range got {
		if d := s.End - s.Start; d > fingerprintWindowSeconds {
			t.Fatalf("stretch %v runs %.2fs, longer than the %ds cap", s, d, fingerprintWindowSeconds)
		}
	}
	if got[0].Start != 2 {
		t.Fatalf("the longest stretch should come first: %v", got)
	}
}
