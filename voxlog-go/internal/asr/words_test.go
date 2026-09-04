package asr

import "testing"

func TestJoinTokensBuildsWholeWordsWithTheirStartTimes(t *testing.T) {
	// SentencePiece, as Parakeet emits it: the marker opens a word, bare
	// pieces continue it.
	words := joinTokens(
		[]string{"▁при", "віт", "▁світ"},
		[]float32{0.2, 0.4, 0.9})
	if len(words) != 2 {
		t.Fatalf("got %d words, want 2: %+v", len(words), words)
	}
	if words[0].Text != "привіт" || words[0].Start != 0.2 {
		t.Errorf("first word = %+v, want {привіт 0.2}", words[0])
	}
	if words[1].Text != "світ" || words[1].Start != 0.9 {
		t.Errorf("second word = %+v, want {світ 0.9}", words[1])
	}
}

func TestJoinTokensHandlesWhisperSpacesAndBareMarkers(t *testing.T) {
	words := joinTokens(
		[]string{" hello", "▁", "world"},
		[]float32{0, 0.5, 0.6})
	if len(words) != 2 || words[0].Text != "hello" || words[1].Text != "world" {
		t.Fatalf("got %+v, want hello then world", words)
	}
	if words[1].Start != 0.6 {
		t.Errorf("second word starts at %v, want 0.6", words[1].Start)
	}
}

// An engine with no timestamps must come back empty rather than with words
// timed at zero: the caller falls back to cutting the audio, and words that
// all claim to start at the same moment would be assigned to one speaker.
func TestJoinTokensRefusesUntimedTokens(t *testing.T) {
	if got := joinTokens([]string{"▁one", "▁two"}, nil); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
	if got := joinTokens(nil, []float32{0.1}); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}
