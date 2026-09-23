package asr

import "fmt"

// Transcriber converts PCM audio (float32, mono, 16kHz) to text.
type Transcriber interface {
	// Transcribe consumes a full utterance and returns final text.
	// For a streaming engine this feeds the whole buffer through in one
	// call and returns the final result.
	Transcribe(samples []float32, language string) (string, error)
	Close()
}

// WordTranscriber is additionally implemented by engines that can say when
// each word was spoken (currently: the offline recognizers, i.e. Parakeet and
// Whisper). Diarization uses it to label speakers without cutting the audio
// up first -- see joinTokens.
type WordTranscriber interface {
	Transcriber
	// TranscribeWords decodes a full utterance and returns its words with
	// their start times. A nil result with a nil error means this engine or
	// this model reported no timings, and the caller has to manage without.
	TranscribeWords(samples []float32, language string) ([]Word, error)
}

// StreamingTranscriber is additionally implemented by engines that can
// report partial text before the utterance ends (currently: Nemotron).
type StreamingTranscriber interface {
	Transcriber
	// Feed pushes one chunk of audio and returns the current partial text.
	Feed(samples []float32) (partial string, err error)
	// Finish signals end of utterance and returns final text.
	Finish() (final string, err error)
}

// NewTranscriber builds the engine matching m.Family, loading model files
// from modelDir (as laid out by ModelDir/Download).
func NewTranscriber(m ModelSpec, modelDir, language string) (Transcriber, error) {
	if !dirHasFiles(modelDir, m) {
		return nil, fmt.Errorf("model %s-%s not downloaded at %s", m.Family, m.Variant, modelDir)
	}

	switch {
	case offlineFamilies[m.Family]:
		return newOfflineTranscriber(m, modelDir, language)
	case onlineFamilies[m.Family]:
		return newOnlineTranscriber(m, modelDir, language)
	default:
		return nil, fmt.Errorf("unknown model family %q", m.Family)
	}
}

// offlineFamilies and onlineFamilies are the families each engine can load:
// offline.go decodes a whole recording in one pass, online.go streams. They
// are maps rather than switch cases so HasEngine can answer from the same
// list NewTranscriber routes by -- main.go's download catalog is a separate
// list, and a model offered there with no engine behind it looks like a
// working download followed by every dictation failing.
var (
	offlineFamilies = map[string]bool{"whisper": true, "parakeet": true, "orukeet": true}
	onlineFamilies  = map[string]bool{"nemotron": true}
)

// HasEngine reports whether NewTranscriber can build an engine for family.
func HasEngine(family string) bool {
	return offlineFamilies[family] || onlineFamilies[family]
}

func dirHasFiles(modelDir string, m ModelSpec) bool {
	for _, f := range m.Files {
		if !fileExists(modelDir, f.Filename) {
			return false
		}
	}
	return len(m.Files) > 0
}
