package voiceprint

import (
	"math"
	"testing"
)

// A stand-in for a voice: a direction in space, with a little noise so no two
// samples of the same person are identical.
func voice(angle, jitter float64) []float32 {
	return Normalize([]float32{
		float32(math.Cos(angle) + jitter*0.01),
		float32(math.Sin(angle) + jitter*0.01),
		float32(jitter * 0.02),
	})
}

func TestCosineOfIdenticalVectorsIsOne(t *testing.T) {
	v := Normalize([]float32{1, 2, 3})
	if got := Cosine(v, v); math.Abs(float64(got)-1) > 1e-6 {
		t.Fatalf("got %v, want 1", got)
	}
}

func TestCosineOfMismatchedOrMissingVectorsIsZero(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Fatalf("different lengths: got %v, want 0", got)
	}
	if got := Cosine(nil, []float32{1, 0}); got != 0 {
		t.Fatalf("missing vector: got %v, want 0", got)
	}
}

func TestNormalizeOfAZeroVectorIsNothing(t *testing.T) {
	if Normalize([]float32{0, 0, 0}) != nil {
		t.Fatal("a zero vector has no direction and must not pretend to")
	}
}

// A profile built from twenty minutes of one recording and two seconds of
// another must sound like the twenty minutes.
func TestWeightedCentroidFollowsTheWeights(t *testing.T) {
	long := voice(0, 0)
	short := voice(math.Pi/2, 0)
	got := WeightedCentroid([][]float32{long, short}, []float64{1200, 2})
	if Cosine(got, long) <= Cosine(got, short) {
		t.Fatalf("centroid leans towards the two-second sample: %v", got)
	}
}

// The case the whole package exists for: one meeting, decoded in blocks, with
// two people alternating. Every block sees both, and numbers them however it
// likes.
func TestLinkBlocksJoinsTheSamePersonAcrossBlocks(t *testing.T) {
	speakers := []BlockSpeaker{
		{Block: 0, Local: 0, Embed: voice(0, 1), Secs: 20},
		{Block: 0, Local: 1, Embed: voice(2, 1), Secs: 15},
		// Second block numbers them the other way round, which is exactly
		// what the diarizer does today.
		{Block: 1, Local: 0, Embed: voice(2, 2), Secs: 18},
		{Block: 1, Local: 1, Embed: voice(0, 2), Secs: 25},
		{Block: 2, Local: 0, Embed: voice(0, 3), Secs: 30},
	}

	ids := LinkBlocks(speakers, MergeThreshold)
	if ids[0] != ids[3] || ids[0] != ids[4] {
		t.Fatalf("the first voice was split across blocks: %v", ids)
	}
	if ids[1] != ids[2] {
		t.Fatalf("the second voice was split across blocks: %v", ids)
	}
	if ids[0] == ids[1] {
		t.Fatalf("two different people were merged: %v", ids)
	}
	// Numbered by first appearance, so the transcript still reads in order.
	if ids[0] != 0 || ids[1] != 1 {
		t.Fatalf("ids are not in first-appearance order: %v", ids)
	}
}

func TestLinkBlocksFoldsAMomentaryClusterIntoItsNeighbour(t *testing.T) {
	speakers := []BlockSpeaker{
		{Block: 0, Local: 0, Embed: voice(0, 1), Secs: 40},
		{Block: 1, Local: 0, Embed: voice(0, 9), Secs: 30},
		// Half a second of the same person, seen slightly differently. It
		// must not become a third participant.
		{Block: 2, Local: 1, Embed: voice(0.35, 4), Secs: 0.5},
	}
	ids := LinkBlocks(speakers, MergeThreshold)
	seen := map[int]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("got %d speakers, want 1: %v", len(seen), ids)
	}
}

// A stretch with no usable fingerprint (too short to embed, or the models
// were not ready) must stay its own speaker rather than being guessed into
// somebody else's identity.
func TestLinkBlocksKeepsUnfingerprintedSpeakersApart(t *testing.T) {
	speakers := []BlockSpeaker{
		{Block: 0, Local: 0, Embed: voice(0, 1), Secs: 30},
		{Block: 1, Local: 0, Secs: 30},
	}
	ids := LinkBlocks(speakers, MergeThreshold)
	if ids[0] == ids[1] {
		t.Fatalf("a voice with no fingerprint was folded into a known one: %v", ids)
	}
}

// ...but they share that id with each other. Each of them standing alone is
// what turned a five-person call into twenty speakers: every block where
// somebody said two words produced a new person.
func TestLinkBlocksPoolsUnfingerprintedSpeakers(t *testing.T) {
	speakers := []BlockSpeaker{
		{Block: 0, Local: 0, Embed: voice(0, 1), Secs: 30},
		{Block: 1, Local: 0, Secs: 0.4},
		{Block: 2, Local: 1, Secs: 0.3},
		{Block: 3, Local: 0, Secs: 0.5},
	}
	ids := LinkBlocks(speakers, MergeThreshold)
	if ids[1] != ids[2] || ids[2] != ids[3] {
		t.Fatalf("three unheard fragments became %v, want one shared id", ids)
	}
	if ids[0] == ids[1] {
		t.Fatalf("the unheard pool swallowed a speaker with a fingerprint: %v", ids)
	}
	seen := map[int]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Fatalf("got %d speakers, want 2 (one person, one unheard pool): %v", len(seen), ids)
	}
}

// A few seconds of somebody is judged more loosely than a full speaker would
// be: its centroid is the least reliable thing in the meeting, and at the
// merge threshold exactly it stayed a person of its own.
func TestLinkBlocksFoldsAFragmentItIsNotQuiteSureAbout(t *testing.T) {
	near := Normalize([]float32{0.52, 0.85, 0})
	speakers := []BlockSpeaker{
		{Block: 0, Local: 0, Embed: voice(0, 1), Secs: 60},
		{Block: 1, Local: 0, Embed: near, Secs: 1.5},
	}
	if got := Cosine(voice(0, 1), near); got >= MergeThreshold || got < MergeThreshold-FoldSlack {
		t.Fatalf("the test's own vectors score %v, which is not inside the fold band", got)
	}
	ids := LinkBlocks(speakers, MergeThreshold)
	if ids[0] != ids[1] {
		t.Fatalf("a 1.5s fragment stayed its own speaker: %v", ids)
	}
}

func TestLinkBlocksHandlesNothing(t *testing.T) {
	if got := LinkBlocks(nil, MergeThreshold); len(got) != 0 {
		t.Fatalf("got %v, want no ids", got)
	}
}
