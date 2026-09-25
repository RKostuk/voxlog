// Package voiceid turns a stretch of speech into a fingerprint. Turning
// fingerprints into people is internal/voiceprint, which is pure arithmetic
// and free of the model this package loads.
//
// Diarization (internal/diarize) answers "who spoke when" inside one stretch
// of audio, and its speaker numbers mean nothing outside it: a meeting is
// decoded in sixty-second blocks, so without this package the same colleague
// is Speaker 1 in one block and Speaker 3 in the next. Embeddings are what
// join those back together -- first across the blocks of one meeting, then
// across meetings, which is what lets a voice carry a name.
package voiceid

import (
	"fmt"
	"path/filepath"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"

	"voxlog-go/internal/audio"
	"voxlog-go/internal/voiceprint"
)

// Embedder turns speech into a fixed-length vector. It uses the same
// wespeaker CAM++ model the diarizer already downloads as half of
// diarize.Spec, just driven directly instead of through the clustering the
// diarizer does internally -- so there is nothing new to download.
type Embedder struct {
	impl *sherpa.SpeakerEmbeddingExtractor
	dim  int
}

// New loads the embedding model from modelDir, the same directory
// asr.ModelDir lays out for diarize.Spec.
func New(modelDir string, numThreads int) (*Embedder, error) {
	cfg := sherpa.SpeakerEmbeddingExtractorConfig{
		Model:      filepath.Join(modelDir, "embedding.onnx"),
		NumThreads: numThreads,
		Provider:   "cpu",
	}
	// Same nil-means-rejected contract as the recognizers and the diarizer:
	// sherpa-onnx logs the offending file to stderr and hands back nil rather
	// than an error.
	impl := sherpa.NewSpeakerEmbeddingExtractor(&cfg)
	if impl == nil {
		return nil, fmt.Errorf("sherpa-onnx rejected the embedding model in %s (see stderr)", modelDir)
	}
	return &Embedder{impl: impl, dim: impl.Dim()}, nil
}

func (e *Embedder) Dim() int { return e.dim }

// Compute returns the L2-normalised fingerprint of samples, or nil when there
// is not enough speech to fingerprint. Normalised here, once, so no caller has
// to remember.
//
// Deliberately NOT centred (see center.go). What is stored against a meeting
// or a named voice is the model's own output, and centring happens in
// Similarity, at the moment two of them are compared -- so a better cohort
// mean improves every comparison the app makes, including ones against
// fingerprints recorded before it existed, instead of splitting the database
// into vectors from before the change and vectors from after.
//
// The stream is created and destroyed inside this call on purpose. The decode
// queue can run a second decode nested inside a long one (see the yield in
// decodequeue.go), and while that is serial rather than concurrent, a live
// stream held across such a call would be a live cgo object held across
// re-entry -- exactly the shape that has crashed this app before.
func (e *Embedder) Compute(samples []float32) []float32 {
	if len(samples) < minEmbedSamples {
		return nil
	}

	stream := e.impl.CreateStream()
	defer sherpa.DeleteOnlineStream(stream)

	stream.AcceptWaveform(audio.SampleRate, samples)
	stream.InputFinished()
	if !e.impl.IsReady(stream) {
		return nil
	}
	return voiceprint.Normalize(e.impl.Compute(stream))
}

func (e *Embedder) Close() {
	if e == nil || e.impl == nil {
		return
	}
	sherpa.DeleteSpeakerEmbeddingExtractor(e.impl)
	e.impl = nil
}

// minEmbedSamples is the shortest stretch worth fingerprinting. CAM++ will
// happily return a vector for a third of a second of audio, and that vector
// is noise -- it matches everybody and nobody, which is worse than having no
// fingerprint at all.
const minEmbedSamples = audio.SampleRate // one second
