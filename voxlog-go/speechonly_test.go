package main

import (
	"testing"

	"voxlog-go/internal/audio"
)

func secs(f float64) int64 { return int64(f * audio.SampleRate) }

// The rule is "cut the middle out of a long silence", not "keep only what
// the gate marked". Measured against real recordings, the second one loses
// sentences the gate never marked -- which is the bug this was fixing.
func TestOnlyLongSilencesAreCut(t *testing.T) {
	// Speech, a two-second breath, more speech: the breath stays.
	spans := mergeSpeechSpans([]speechSpan{
		{secs(10), secs(20)},
		{secs(22), secs(30)},
	}, secs(120))
	if len(spans) != 1 {
		t.Fatalf("a two-second pause split the recording into %d pieces", len(spans))
	}
	if spans[0].start != secs(9) || spans[0].end != secs(31) {
		t.Fatalf("got %v..%v, want the two stretches joined and padded", spans[0].start, spans[0].end)
	}
}

func TestASilenceLongerThanTheLimitIsCut(t *testing.T) {
	spans := mergeSpeechSpans([]speechSpan{
		{secs(10), secs(20)},
		{secs(60), secs(70)},
	}, secs(120))
	if len(spans) != 2 {
		t.Fatalf("a forty-second silence left %d piece(s), want it cut", len(spans))
	}
	if kept := speechSeconds(spans); kept > 25 {
		t.Fatalf("%.1fs kept out of two ten-second stretches -- the silence is still in there", kept)
	}
}

// A recording that opens on the first word and ends shortly after the last
// one is kept whole: there is no dead air to cut, and trimming flush against
// the speech clips consonants.
func TestTheHeadAndTailAreKeptWhenTheyAreShort(t *testing.T) {
	spans := mergeSpeechSpans([]speechSpan{{secs(2), secs(28)}}, secs(30))
	if len(spans) != 1 {
		t.Fatalf("got %d pieces, want one", len(spans))
	}
	if spans[0].start != 0 || spans[0].end != secs(30) {
		t.Fatalf("got %v..%v, want the whole recording", spans[0].start, spans[0].end)
	}
}

// Nothing marked means nothing to decode. This is what stops a recording of
// an empty room being handed to a recognizer that will invent a paragraph
// out of the hiss -- which is exactly what it did.
func TestSilenceAloneProducesNoSpans(t *testing.T) {
	if spans := mergeSpeechSpans(nil, secs(60)); spans != nil {
		t.Fatalf("got %v, want nothing to decode", spans)
	}
}

// Padding must never reach past the recording, or the read comes back short
// and the caller is decoding a different stretch than it asked for.
func TestPaddingIsClampedToTheRecording(t *testing.T) {
	spans := mergeSpeechSpans([]speechSpan{{0, secs(5)}}, secs(5))
	if len(spans) != 1 || spans[0].start != 0 || spans[0].end != secs(5) {
		t.Fatalf("got %v, want exactly the recording", spans)
	}
}
