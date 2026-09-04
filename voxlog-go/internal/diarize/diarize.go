// Package diarize splits a recording into per-speaker time ranges, so the
// far end of a call can be transcribed as several people instead of one
// "Call:" block. It answers "who spoke when", not "who is speaking" -- the
// speakers come back as numbers, in the order they first talk.
package diarize

import (
	"fmt"
	"path/filepath"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"

	"voxlog-go/internal/asr"
)

// Spec is the model pair diarization needs, laid out for asr.Download so it
// reuses the same download, resume and "is it there" logic as the ASR models.
//
// Two models, because the job is two jobs: pyannote's segmentation network
// finds where speech is and where speakers change, and the embedding network
// turns each of those pieces into a vector that can be clustered, which is
// what decides that piece 1 and piece 4 are the same person.
var Spec = asr.ModelSpec{
	Family:      "diarization",
	Variant:     "pyannote-cam++",
	Description: "Tells apart speakers on the far end of a call · ~35 MB",
	Files: []asr.ModelFile{
		{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-pyannote-segmentation-3-0/resolve/main/model.onnx", Filename: "segmentation.onnx"},
		{URL: "https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-recongition-models/wespeaker_en_voxceleb_CAM%2B%2B.onnx", Filename: "embedding.onnx"},
	},
}

// Segment is one continuous stretch of one speaker, in seconds from the
// start of the audio.
type Segment struct {
	Start   float32
	End     float32
	Speaker int
}

type Diarizer struct {
	impl *sherpa.OfflineSpeakerDiarization
}

// New loads the diarization models from modelDir (as laid out by
// asr.ModelDir/Download).
func New(modelDir string, numThreads int) (*Diarizer, error) {
	cfg := sherpa.OfflineSpeakerDiarizationConfig{
		Segmentation: sherpa.OfflineSpeakerSegmentationModelConfig{
			Pyannote:   sherpa.OfflineSpeakerSegmentationPyannoteModelConfig{Model: filepath.Join(modelDir, "segmentation.onnx")},
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		Embedding: sherpa.SpeakerEmbeddingExtractorConfig{
			Model:      filepath.Join(modelDir, "embedding.onnx"),
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		// NumClusters -1 means "decide from the data": nobody knows how many
		// people are on a call before it is recorded, so the count comes from
		// the threshold instead. Lower splits one person into several, higher
		// merges two people into one; 0.5 is sherpa's own default and errs
		// toward merging, which reads better than a transcript that invents
		// speakers.
		Clustering:     sherpa.FastClusteringConfig{NumClusters: -1, Threshold: 0.5},
		MinDurationOn:  0.3,
		MinDurationOff: 0.5,
	}

	// Same nil-means-rejected contract as the recognizers: sherpa-onnx logs
	// the offending file to stderr and hands back nil rather than an error.
	impl := sherpa.NewOfflineSpeakerDiarization(&cfg)
	if impl == nil {
		return nil, fmt.Errorf("sherpa-onnx rejected the diarization models in %s (see stderr for the specific file)", modelDir)
	}
	return &Diarizer{impl: impl}, nil
}

// Process returns the speaker segments in start-time order. An empty result
// means it found no speech worth splitting, which the caller should treat as
// "transcribe it as one block" rather than as an error.
func (d *Diarizer) Process(samples []float32) []Segment {
	if len(samples) == 0 {
		// sherpa's Process indexes samples[0] with no length check of its own.
		return nil
	}

	raw := d.impl.Process(samples)
	out := make([]Segment, 0, len(raw))
	for _, s := range raw {
		out = append(out, Segment{Start: s.Start, End: s.End, Speaker: s.Speaker})
	}
	return out
}

func (d *Diarizer) Close() {
	sherpa.DeleteOfflineSpeakerDiarization(d.impl)
}

// Merge joins neighbouring segments from the same speaker, and drops any
// stretch too short to transcribe usefully.
//
// Both matter for what happens next: every segment costs one pass through the
// ASR model, and a 200ms sliver decodes to junk or to nothing at all. The gap
// tolerance is what keeps one person's natural pauses from becoming a dozen
// separate labeled lines.
func Merge(segments []Segment, maxGap, minDuration float32) []Segment {
	out := make([]Segment, 0, len(segments))
	for _, s := range segments {
		if n := len(out); n > 0 && out[n-1].Speaker == s.Speaker && s.Start-out[n-1].End <= maxGap {
			out[n-1].End = s.End
			continue
		}
		out = append(out, s)
	}

	kept := out[:0]
	for _, s := range out {
		if s.End-s.Start >= minDuration {
			kept = append(kept, s)
		}
	}
	return kept
}

// Slice returns the samples covered by a segment, clamped to what the buffer
// actually holds -- the segmentation model works on its own frame grid and
// can report an end a few milliseconds past the last sample.
func Slice(samples []float32, s Segment, sampleRate int) []float32 {
	start := int(s.Start * float32(sampleRate))
	end := int(s.End * float32(sampleRate))
	if start < 0 {
		start = 0
	}
	if end > len(samples) {
		end = len(samples)
	}
	if start >= end {
		return nil
	}
	return samples[start:end]
}
