package asr

import (
	"fmt"
	"path/filepath"
	"runtime"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"
)

const sampleRate = 16000

// offlineTranscriber wraps sherpa-onnx's non-streaming recognizer, used for
// Whisper and Parakeet (transducer) model families.
type offlineTranscriber struct {
	recognizer *sherpa.OfflineRecognizer
}

// newOfflineTranscriber builds the recognizer. language matters here (not
// at Transcribe time) because Whisper takes it as recognizer-level config:
// switching languages means rebuilding, which transcriberCache handles by
// keying on the language too.
func newOfflineTranscriber(m ModelSpec, modelDir, language string) (*offlineTranscriber, error) {
	cfg := sherpa.OfflineRecognizerConfig{
		ModelConfig: sherpa.OfflineModelConfig{
			Tokens:     filepath.Join(modelDir, "tokens.txt"),
			NumThreads: numThreads(),
			Provider:   "cpu",
		},
		DecodingMethod: "greedy_search",
	}

	// No Canary branch, though sherpa-onnx has one and the Go bindings expose
	// it: its config validator only accepts en, de, es or fr as the source
	// language, in the current master as well as in the version linked here,
	// because that is what the 180m-flash export covers. Pointing it at the
	// canary-1b-v2 export (which does cover Ukrainian) and leaving the
	// language empty passes validation, but the model then TRANSLATES --
	// Ukrainian speech came back as English prose. A dictation app cannot
	// ship a model that silently rewrites what was said in another language.

	if m.Family == "whisper" {
		cfg.ModelConfig.Whisper = sherpa.OfflineWhisperModelConfig{
			Encoder: filepath.Join(modelDir, "encoder.onnx"),
			Decoder: filepath.Join(modelDir, "decoder.onnx"),
			// Empty means auto-detect; a bare ISO code ("uk", "en") pins it.
			// An .en-only build ignores this either way -- hence the
			// SupportsLanguage flag gating the picker in Settings.
			Language: whisperLanguage(language),
			Task:     "transcribe",
		}
	} else {
		cfg.ModelConfig.Transducer = sherpa.OfflineTransducerModelConfig{
			Encoder: filepath.Join(modelDir, "encoder.onnx"),
			Decoder: filepath.Join(modelDir, "decoder.onnx"),
			Joiner:  filepath.Join(modelDir, "joiner.onnx"),
		}
	}

	// Same nil-means-rejected contract as the online path: sherpa-onnx
	// logs the offending file to stderr and returns nil rather than an
	// error, and using that nil downstream segfaults the process.
	recognizer := sherpa.NewOfflineRecognizer(&cfg)
	if recognizer == nil {
		return nil, fmt.Errorf("sherpa-onnx rejected the offline model in %s (see stderr for the specific file)", modelDir)
	}
	return &offlineTranscriber{recognizer: recognizer}, nil
}

// TranscribeWords decodes the same way Transcribe does and keeps the token
// timings the result already carries, which Transcribe throws away. One pass
// over a whole minute of audio, then the words are sorted out afterwards --
// see asr.Word for why that beats decoding a speaker's turn at a time.
func (t *offlineTranscriber) TranscribeWords(samples []float32, language string) ([]Word, error) {
	result, err := t.decode(samples)
	if err != nil || result == nil {
		return nil, err
	}
	return joinTokens(result.Tokens, result.Timestamps), nil
}

func (t *offlineTranscriber) Transcribe(samples []float32, language string) (string, error) {
	result, err := t.decode(samples)
	if err != nil || result == nil {
		return "", err
	}
	return result.Text, nil
}

// decode runs one pass of the recognizer. A nil result with a nil error is an
// empty input, which is not a failure -- see the length check below.
func (t *offlineTranscriber) decode(samples []float32) (*sherpa.OfflineRecognizerResult, error) {
	if len(samples) == 0 {
		// sherpa-onnx-go's AcceptWaveform indexes into samples[0] internally
		// (sherpa_onnx.go:892) with no length check of its own -- an empty
		// slice (e.g. a dictate tap released before any audio callback ever
		// fired) panics the whole process instead of returning an error.
		return nil, nil
	}

	stream := sherpa.NewOfflineStream(t.recognizer)
	if stream == nil {
		return nil, fmt.Errorf("could not create an offline decoding stream")
	}
	defer sherpa.DeleteOfflineStream(stream)

	stream.AcceptWaveform(sampleRate, samples)
	t.recognizer.Decode(stream)
	return stream.GetResult(), nil
}

func (t *offlineTranscriber) Close() {
	sherpa.DeleteOfflineRecognizer(t.recognizer)
}

func numThreads() int {
	if n := runtime.NumCPU(); n > 0 && n < 4 {
		return n
	}
	return 4
}

// whisperLanguage maps our setting to Whisper's language field: "auto" (or
// unset) means let Whisper detect it, anything else passes through as the
// ISO code Whisper expects.
func whisperLanguage(language string) string {
	if language == "" || language == "auto" {
		return ""
	}
	return language
}
