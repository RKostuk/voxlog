package asr

import "strings"

// Word is one word of a transcript and the moment it was spoken, in seconds
// from the start of the audio that was decoded.
//
// It exists so speakers can be told apart without cutting the audio into
// per-speaker pieces first: a recognizer given a whole minute at once reads
// far better than one handed a second at a time, and the timings are enough
// to say afterwards who said which word.
type Word struct {
	Text  string
	Start float32
}

// Word-start markers. SentencePiece models (Parakeet) mark the beginning of a
// word with U+2581; Whisper's tokens carry a leading space instead. Anything
// else continues the word before it, which is what makes a token a subword.
const (
	sentencePieceMark = "▁"
	whisperMark       = " "
)

// joinTokens turns sherpa's per-token output into whole words, each carrying
// the timestamp of its first token. Returns nil when the tokens cannot be
// timed -- an engine that reports no timestamps has to be handled by the
// caller, and half-timed words would be worse than none.
func joinTokens(tokens []string, times []float32) []Word {
	if len(tokens) == 0 || len(times) < len(tokens) {
		return nil
	}
	out := make([]Word, 0, len(tokens))
	// The first token starts a word whether or not it is marked: a result
	// beginning mid-word is not a thing sherpa produces, and treating it as a
	// continuation would mean appending to a word that does not exist.
	starts := true
	for i, tok := range tokens {
		piece := tok
		if strings.HasPrefix(tok, sentencePieceMark) || strings.HasPrefix(tok, whisperMark) {
			piece = strings.TrimPrefix(strings.TrimPrefix(tok, sentencePieceMark), whisperMark)
			starts = true
		}
		// A bare marker is a word boundary with no letters of its own; it must
		// break the next token off rather than be glued to the previous word.
		if piece == "" {
			continue
		}
		if starts {
			out = append(out, Word{Text: piece, Start: times[i]})
			starts = false
			continue
		}
		out[len(out)-1].Text += piece
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
