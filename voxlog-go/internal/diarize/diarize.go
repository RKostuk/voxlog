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
	"voxlog-go/internal/voiceprint"
)

// Spec is the model pair diarization needs, laid out for asr.Download so it
// reuses the same download, resume and "is it there" logic as the ASR models.
//
// Two models, because the job is two jobs: pyannote's segmentation network
// finds where speech is and where speakers change, and the embedding network
// turns each of those pieces into a vector that can be clustered, which is
// what decides that piece 1 and piece 4 are the same person.
// The variant is part of the directory name, and it changes whenever the
// files behind it do: a download is considered present if the filenames are
// on disk, so a new model at the old name would never be fetched.
var Spec = asr.ModelSpec{
	Family:      "diarization",
	Variant:     "pyannote-cnceleb34",
	Description: "Tells apart speakers on the far end of a call · ~33 MB",
	Files: []asr.ModelFile{
		{URL: "https://huggingface.co/csukuangfj/sherpa-onnx-pyannote-segmentation-3-0/resolve/main/model.onnx", Filename: "segmentation.onnx"},
		// The embedding model was wespeaker's English VoxCeleb CAM++ until it
		// was measured against a recording whose four speakers were known by
		// ear. It scored 0.58 of labelled speech attributed to the right
		// person -- it merged two people and split a third -- against 0.82 for
		// this one, which is also the smaller file. Six models were compared;
		// see docs/diarization-calibration.md. The language is most of the
		// story: nothing in this app is recorded in English.
		{URL: "https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-recongition-models/wespeaker_zh_cnceleb_resnet34_LM.onnx", Filename: "embedding.onnx"},
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
	link float32
}

// Options are the diarization knobs worth tuning from data rather than
// guessing. A zero field means "the package default", so a caller that cares
// about one of them does not have to restate the other three.
//
// They exist as a type because the defaults below are not self-evidently
// right: they are measured, by tools/diareval, against recordings whose
// speakers are known. Without a way to vary them from outside, that
// measurement would have to be done by editing this file.
type Options struct {
	// ClusterThreshold is how similar two stretches must sound to be called
	// the same person. Lower splits one person into several, higher merges
	// two people into one.
	ClusterThreshold float32
	// NumClusters fixes how many people are in the recording. -1, and the
	// default, is "decide from the data" -- nobody knows the count before a
	// call is recorded. Set it only when something does know.
	NumClusters int
	// MinDurationOn is the shortest stretch of speech the segmentation model
	// is allowed to report, and MinDurationOff the shortest silence that is
	// allowed to end one.
	MinDurationOn  float32
	MinDurationOff float32
	// EmbeddingModel replaces the embedding half of Spec with another file.
	// Which speaker model does the clustering is the single biggest lever on
	// whether four voices come back as four, and it is not a lever anyone
	// should pull without measuring: tools/diareval exists to compare them
	// against a recording whose speakers are known.
	EmbeddingModel string
	// LinkThreshold is how alike two of the model's own speakers have to be,
	// in the centred space of internal/voiceprint, to be folded into one
	// person by Windowed. The model's clustering above and this are two
	// different questions asked of two different comparisons, which is why
	// they are two numbers.
	LinkThreshold float32
}

// Defaults are the shipping settings, and the baseline every sweep in
// tools/diareval is measured against.
var Defaults = Options{
	// 0.6 rather than sherpa's own 0.5, measured with tools/diareval against a
	// recording whose four speakers were labelled by ear: 0.6 returns four,
	// and 0.65 returns three by folding the shortest voice into another. It is
	// one recording and 68 seconds of labelled speech, so this is a ranking
	// that holds and a second decimal place that does not -- treat it as
	// provisional until a second reference exists.
	//
	// Both directions cost something, but not equally: a person split in two
	// is visible and mergeable in the Voices pane, while two people merged
	// into one is a transcript that quietly lies about who said what.
	ClusterThreshold: 0.6,
	NumClusters:      -1,
	MinDurationOn:    0.3,
	MinDurationOff:   0.5,
	LinkThreshold:    voiceprint.MergeThreshold,
}

// embeddingModel is the file the clustering compares voices with: the one
// beside the segmentation model, unless a caller names another.
func embeddingModel(modelDir, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(modelDir, "embedding.onnx")
}

// withDefaults fills in the zero fields. NumClusters is the odd one: 0 is not
// a meaningful count, so it reads as "unset" the same way the others do.
func (o Options) withDefaults() Options {
	if o.ClusterThreshold == 0 {
		o.ClusterThreshold = Defaults.ClusterThreshold
	}
	if o.NumClusters == 0 {
		o.NumClusters = Defaults.NumClusters
	}
	if o.MinDurationOn == 0 {
		o.MinDurationOn = Defaults.MinDurationOn
	}
	if o.MinDurationOff == 0 {
		o.MinDurationOff = Defaults.MinDurationOff
	}
	if o.LinkThreshold == 0 {
		o.LinkThreshold = Defaults.LinkThreshold
	}
	return o
}

// New loads the diarization models from modelDir (as laid out by
// asr.ModelDir/Download), with the shipping settings.
func New(modelDir string, numThreads int) (*Diarizer, error) {
	return NewWithOptions(modelDir, numThreads, Options{})
}

// NewWithOptions is New with the tuning exposed. Only tools/diareval and its
// tests pass anything but the zero Options; the app takes the defaults.
func NewWithOptions(modelDir string, numThreads int, o Options) (*Diarizer, error) {
	o = o.withDefaults()
	cfg := sherpa.OfflineSpeakerDiarizationConfig{
		Segmentation: sherpa.OfflineSpeakerSegmentationModelConfig{
			Pyannote:   sherpa.OfflineSpeakerSegmentationPyannoteModelConfig{Model: filepath.Join(modelDir, "segmentation.onnx")},
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		Embedding: sherpa.SpeakerEmbeddingExtractorConfig{
			Model:      embeddingModel(modelDir, o.EmbeddingModel),
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		// NumClusters -1 means "decide from the data": nobody knows how many
		// people are on a call before it is recorded, so the count comes from
		// the threshold instead.
		Clustering:     sherpa.FastClusteringConfig{NumClusters: o.NumClusters, Threshold: o.ClusterThreshold},
		MinDurationOn:  o.MinDurationOn,
		MinDurationOff: o.MinDurationOff,
	}

	// Same nil-means-rejected contract as the recognizers: sherpa-onnx logs
	// the offending file to stderr and hands back nil rather than an error.
	impl := sherpa.NewOfflineSpeakerDiarization(&cfg)
	if impl == nil {
		return nil, fmt.Errorf("sherpa-onnx rejected the diarization models in %s (see stderr for the specific file)", modelDir)
	}
	return &Diarizer{impl: impl, link: o.LinkThreshold}, nil
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

// LinkThreshold is the setting Windowed folds the model's own speakers
// together at, after defaults have been applied.
func (d *Diarizer) LinkThreshold() float32 { return d.link }

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
