package main

import (
	"log"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/vad"
)

// Feeding a recognizer the silence between sentences.
//
// An always-on recording is mostly not speech. It opens on the first word
// and runs until the room has been quiet for the whole silence gap, so a
// twenty-second remark arrives as a fifty-second file with thirty seconds of
// an empty room in it -- and when the room is a call that has gone quiet,
// minutes of it.
//
// Handed that, parakeet does one of two things, and both were in the user's
// history: it returns a fraction of what was said, or it invents a paragraph
// out of the hiss. The second one is worse, because it looks like a
// transcript. Neither is a decoding bug -- there is no cap or early exit
// anywhere in the block loop; it is what a speech model does when most of
// what it is given is not speech.
//
// So the silence is taken out before the recognizer sees it. The same gate
// that decided to record is run over the finished file, and only the
// stretches it calls speech are decoded, in order, with a little of the
// audio either side of each so no word is clipped by the boundary.

const (
	// speechPad is how much audio is kept on each side of a stretch the gate
	// marked, so the first consonant and the last one survive the cut -- and
	// so does anything said just quietly enough that the gate missed it.
	speechPad = 1.0
	// maxSilence is the longest pause kept as it is. Anything up to this is
	// part of the conversation: a breath, a thought, somebody else finishing
	// their sentence. Only silence longer than this is cut, and then only
	// its middle -- speechPad of it survives at each end.
	//
	// Deliberately generous. Measured against the recordings this came from,
	// a tighter cut started losing sentences the gate had not marked: it is
	// a speech model, not an oracle, and the cost of trusting it too far is
	// exactly the bug being fixed. Removing the dead minute is worth doing;
	// shaving the pauses is not.
	maxSilence = 4.0
)

// speechSpan is one stretch of a recording worth decoding, in samples.
type speechSpan struct{ start, end int64 }

// speechSpans runs the voice-activity gate over a finished recording and
// returns the stretches of it that hold speech, merged and padded.
//
// A nil result means the gate is unavailable (no model), which is the
// caller's cue to decode the file whole rather than to decode nothing: a
// missing model must never cost the user their transcript.
func (a *app) speechSpans(path string) ([]speechSpan, bool) {
	gate, err := vad.New(asr.ModelDir(a.modelsDir, vad.Spec), vad.DefaultConfig())
	if err != nil {
		log.Printf("speech spans: %v", err)
		return nil, false
	}
	defer gate.Close()

	r, err := audio.OpenWAV(path)
	if err != nil {
		log.Printf("speech spans: %v", err)
		return nil, false
	}
	defer r.Close()
	total := r.Samples()

	var spans []speechSpan
	add := func(segs []vad.Segment) {
		for _, seg := range segs {
			spans = append(spans, speechSpan{
				start: seg.StartSample,
				end:   seg.StartSample + int64(len(seg.Samples)),
			})
		}
	}
	for {
		block, err := r.Read(audio.BlockSamples)
		if len(block) > 0 {
			add(gate.Feed(block))
		}
		if err != nil || len(block) == 0 {
			break
		}
	}
	// Whatever the gate was still holding when the file ended. Flush emits
	// the stretch in progress, which is the one that runs to the last word
	// -- exactly the part a recording ends on.
	add(gate.Flush())

	return mergeSpeechSpans(spans, total), true
}

// mergeSpeechSpans turns the stretches the gate marked into the stretches
// worth decoding: everything except the middle of a silence longer than
// maxSilence.
//
// The inversion matters. Decoding only what the gate marked means trusting
// it to have marked everything, and it does not -- on these recordings that
// cost whole sentences. Decoding everything except the long silences keeps
// every sample the gate was unsure about and removes only what it was sure
// about, which is the safe direction to be wrong in.
func mergeSpeechSpans(spans []speechSpan, total int64) []speechSpan {
	if len(spans) == 0 {
		return nil
	}
	pad := int64(speechPad * audio.SampleRate)
	gap := int64(maxSilence * audio.SampleRate)

	out := make([]speechSpan, 0, len(spans))
	for _, s := range spans {
		s.start -= pad
		s.end += pad
		if s.start < 0 {
			s.start = 0
		}
		if total > 0 && s.end > total {
			s.end = total
		}
		if s.end <= s.start {
			continue
		}
		if n := len(out); n > 0 && s.start-out[n-1].end <= gap {
			if s.end > out[n-1].end {
				out[n-1].end = s.end
			}
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	// The head and the tail of the file are silences too, and they get the
	// same rule rather than being cut flush against the first and last word.
	if out[0].start <= gap {
		out[0].start = 0
	}
	if last := len(out) - 1; total > 0 && total-out[last].end <= gap {
		out[last].end = total
	}
	return out
}

// speechSeconds is how much of a recording the spans cover.
func speechSeconds(spans []speechSpan) float64 {
	var n int64
	for _, s := range spans {
		n += s.end - s.start
	}
	return float64(n) / audio.SampleRate
}
