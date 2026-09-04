package asr

import (
	"fmt"
	"path/filepath"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"
)

// onlineTranscriber wraps sherpa-onnx's streaming recognizer, used for the
// Nemotron model family. The underlying OnlineStream is created once and
// reused across utterances via Reset (per SHERPA_API_NOTES.md).
type onlineTranscriber struct {
	recognizer *sherpa.OnlineRecognizer
	stream     *sherpa.OnlineStream
	// fed reports whether any real audio has gone into the current
	// utterance yet, so the leading silence pad is written exactly once.
	fed bool
	// language is the setting this engine was built for, applied to every
	// utterance. Held here because the streaming path (Feed/Finish) has no
	// language argument to carry it: the app hands over chunks as they
	// arrive, never a whole utterance with its settings attached.
	language string
}

// edgePadSeconds is how much silence to feed on each side of an utterance.
//
// Nemotron's encoder is cache-aware and streaming: it needs lookback and
// lookahead context to finalize the chunks touching the edges of speech.
// Fed audio that begins on the first spoken sample and ends on the last --
// which is exactly what a push-to-talk recorder produces -- it silently
// drops the boundary words. Padding gives it the margin it expects. Same
// value and reasoning as the Python app's NEMOTRON_EDGE_PAD_SECONDS.
const edgePadSeconds = 0.4

func silence(seconds float64) []float32 {
	return make([]float32, int(seconds*sampleRate))
}

func newOnlineTranscriber(m ModelSpec, modelDir, language string) (*onlineTranscriber, error) {
	cfg := sherpa.OnlineRecognizerConfig{
		ModelConfig: sherpa.OnlineModelConfig{
			Transducer: sherpa.OnlineTransducerModelConfig{
				Encoder: filepath.Join(modelDir, "encoder.onnx"),
				Decoder: filepath.Join(modelDir, "decoder.onnx"),
				Joiner:  filepath.Join(modelDir, "joiner.onnx"),
			},
			Tokens:     filepath.Join(modelDir, "tokens.txt"),
			NumThreads: numThreads(),
			Provider:   "cpu",
			// Nemotron's cache-aware FastConformer-RNNT isn't the plain
			// transducer sherpa-onnx would otherwise infer; naming it here
			// selects OnlineTransducerNemoModel, which is what knows about
			// the encoder's extra prompt_index (language) input.
			ModelType: "nemo_transducer",
		},
		DecodingMethod: "greedy_search",
		// Endpoint detection OFF, deliberately. It exists for open-mic
		// streaming, where the recognizer has to guess where an utterance
		// ends; here the user says so explicitly with the dictate hotkey.
		//
		// Left on, a pause longer than Rule2MinTrailingSilence (1.2s) --
		// i.e. any normal pause between words -- closed the segment, and
		// GetResult then returned only the segment after it. Speaking
		// "one ... two ... three" with pauses yielded just "three": the
		// "it cuts the beginning/end off" symptom, which no amount of edge
		// padding could fix because the words weren't lost in the encoder,
		// they were dropped from the result.
		EnableEndpoint: 0,
	}

	// sherpa-onnx reports a bad config (missing/mismatched model files) by
	// logging to stderr and handing back a nil recognizer -- there is no
	// error return. Passing that nil straight to NewOnlineStream segfaults
	// the whole process, so the nil check IS the error handling here.
	recognizer := sherpa.NewOnlineRecognizer(&cfg)
	if recognizer == nil {
		return nil, fmt.Errorf("sherpa-onnx rejected the online model in %s (see stderr for the specific file)", modelDir)
	}

	stream := sherpa.NewOnlineStream(recognizer)
	if stream == nil {
		sherpa.DeleteOnlineRecognizer(recognizer)
		return nil, fmt.Errorf("could not create an online stream for the model in %s", modelDir)
	}
	return &onlineTranscriber{recognizer: recognizer, stream: stream, language: language}, nil
}

// nemotronLocales maps the app's bare ISO codes to the locale tags
// Nemotron's language prompt expects. The model is conditioned on a
// language index looked up from this tag, so "uk" alone doesn't resolve --
// it wants the full "uk-UA".
var nemotronLocales = map[string]string{
	"uk": "uk-UA",
	"en": "en-US",
}

// nemotronLanguageOption maps our language setting to Nemotron's stream
// "language" option. "auto" (or anything unrecognized) leaves the model in
// its auto-detect mode rather than guessing a locale for it.
func nemotronLanguageOption(language string) string {
	if locale, ok := nemotronLocales[language]; ok {
		return locale
	}
	return "auto"
}

func (t *onlineTranscriber) Feed(samples []float32) (string, error) {
	if !t.fed {
		t.fed = true
		// Pin the language at the start of each utterance. This used to live
		// only in Transcribe, which the live-streaming path never calls -- so
		// with "live streaming text" on, picking Ukrainian did nothing and the
		// model guessed, sometimes landing on Spanish.
		t.stream.SetOption("language", nemotronLanguageOption(t.language))
		t.stream.AcceptWaveform(sampleRate, silence(edgePadSeconds))
	}
	t.stream.AcceptWaveform(sampleRate, samples)
	for t.recognizer.IsReady(t.stream) {
		t.recognizer.Decode(t.stream)
	}
	return t.recognizer.GetResult(t.stream).Text, nil
}

func (t *onlineTranscriber) Finish() (string, error) {
	// Trailing pad before signalling the end, so the final words get the
	// same lookahead context as everything before them.
	t.stream.AcceptWaveform(sampleRate, silence(edgePadSeconds))
	t.stream.InputFinished()
	for t.recognizer.IsReady(t.stream) {
		t.recognizer.Decode(t.stream)
	}
	final := t.recognizer.GetResult(t.stream).Text
	t.recognizer.Reset(t.stream)
	t.fed = false // next utterance needs its own leading pad
	return final, nil
}

func (t *onlineTranscriber) Transcribe(samples []float32, language string) (string, error) {
	// A per-call language wins over the one the engine was built with, so the
	// batch path stays self-contained.
	t.language = language
	if _, err := t.Feed(samples); err != nil {
		return "", err
	}
	return t.Finish()
}

func (t *onlineTranscriber) Close() {
	sherpa.DeleteOnlineStream(t.stream)
	sherpa.DeleteOnlineRecognizer(t.recognizer)
}
