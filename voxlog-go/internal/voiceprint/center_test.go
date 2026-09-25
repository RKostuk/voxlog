package voiceprint

import "testing"

// Center is the whole reason a threshold on Similarity means anything. It has
// to leave a fingerprint alone when it cannot do its job, rather than return
// something that looks like an answer.
func TestCenterLeavesAVectorAloneWhenTheCohortDoesNotFit(t *testing.T) {
	v := Normalize([]float32{1, 2, 3})
	if got := Center(v); !sameVector(got, v) {
		t.Fatalf("a vector of another length was changed: %v", got)
	}
	if got := Center(nil); got != nil {
		t.Fatalf("nil came back as %v", got)
	}
}

func TestCenterRemovesTheSharedDirection(t *testing.T) {
	saved := cohortMean
	defer func() { cohortMean = saved }()

	// Two fingerprints that are mostly the same thing -- the microphone, the
	// codec, the room -- and differ only in their last component, which is the
	// part that is the speaker.
	cohortMean = Normalize([]float32{1, 0, 0})
	a := Normalize([]float32{10, 1, 0})
	b := Normalize([]float32{10, -1, 0})

	if raw := Cosine(a, b); raw < 0.9 {
		t.Fatalf("the shared direction should dominate before centring, got %.3f", raw)
	}
	if got := Similarity(a, b); got > 0 {
		t.Fatalf("after centring these should read as different, got %.3f", got)
	}
}

func sameVector(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
