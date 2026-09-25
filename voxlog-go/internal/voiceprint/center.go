// Package voiceprint is the arithmetic of comparing voice fingerprints, and
// nothing else: no model, no cgo, no files. Both the half of the app that
// produces fingerprints (internal/voiceid, which loads a 35 MB ONNX model) and
// the half that stores them (internal/history, which must not drag that model
// into every test that touches a database) depend on this one.
package voiceprint

// Score normalisation: making a cosine between two fingerprints mean
// something.
//
// A CAM++ vector describes more than a voice. It also carries the microphone,
// the codec, the system-audio tap, the language -- everything two recordings
// made the same way have in common. That shared part is large enough to swamp
// the part that is the person: measured over this app's own recordings
// (tools/diareval -compare), two takes by the same speaker scored 0.66 to
// 0.84, and two different speakers 0.57 to 0.90. The bands overlap completely,
// which is why no value of MergeThreshold ever worked -- there was no signal
// underneath it to threshold.
//
// Subtracting a cohort mean removes most of that shared direction. The same
// pairs then score about +0.4 for one person and -0.4 or below for two, which
// is a gap a threshold can sit in. This is the cheap end of what speaker
// verification calls score normalisation, and it is the difference between a
// meeting with three speakers reading as three people and reading as one.

// Center returns v with the cohort mean removed and renormalised -- the space
// every comparison in this package happens in.
//
// A vector that cannot be centred (no cohort compiled in, or a cohort from a
// different model) comes back unchanged rather than nil. That is the honest
// fallback: comparisons are then as weak as they were before this existed,
// which is bad, but they still happen.
func Center(v []float32) []float32 { return remove(v, cohortMean) }

// remove subtracts one direction from a fingerprint and renormalises. A vector
// that cannot be centred against mean -- no mean, or a mean of another length,
// which is what a changed model looks like -- comes back unchanged.
//
// Only the cohort mean is ever removed. Subtracting a recording's OWN mean
// separates its speakers far better -- on a three-voice call it moved
// different people from 0.89 apart to -0.32 -- but it is not a safe default:
// with everyone in the recording being one person, the mean is that person,
// and what is left of each fingerprint is noise pointing in every direction,
// so one speaker heard in three places becomes three people. The clustering
// inside the model decides that question instead, over a whole recording at a
// time, and the comparisons here only join what it could not see across.
func remove(v, mean []float32) []float32 {
	if len(v) == 0 || len(v) != len(mean) {
		return v
	}
	out := make([]float32, len(v))
	for i := range v {
		out[i] = v[i] - mean[i]
	}
	if n := Normalize(out); n != nil {
		return n
	}
	return v
}

// Similarity is how alike two fingerprints are, and the only comparison the
// rest of the app should make. It is Cosine over centred vectors: the same
// arithmetic, on the part of a fingerprint that is actually the speaker.
//
// Every threshold in this package is a threshold on this, not on Cosine.
func Similarity(a, b []float32) float32 { return Cosine(Center(a), Center(b)) }

// Centered reports whether a cohort is compiled in at all, so a caller can say
// why its comparisons are the weak ones.
func Centered() bool { return len(cohortMean) > 0 }
