// Package vad answers one question about a stream of microphone samples: is
// somebody talking right now, and if so, where did that stretch of speech
// start and end.
//
// It is the gate in front of always-on recording. Recording everything and
// sorting it out later is the design this app deliberately does not have --
// the machine belongs to the user, and hours of keyboard noise and music
// are neither worth the disk nor worth the decode time.
package vad

import (
	"fmt"
	"path/filepath"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
)

// Spec is silero-vad, laid out for asr.Download so it reuses the same
// download, resume and "is it on disk" logic as every other model. One small
// file -- this is the cheapest model in the app by two orders of magnitude,
// which is what makes it usable as a gate that runs all day.
var Spec = asr.ModelSpec{
	Family:      "vad",
	Variant:     "silero",
	Description: "Hears when someone is actually speaking · ~2 MB",
	Files: []asr.ModelFile{
		{URL: "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx", Filename: "silero_vad.onnx"},
	},
}

// windowSize is silero's own frame size at 16 kHz. Not a tuning knob: the
// model is exported for exactly this many samples per step.
const windowSize = 512

// bufferSeconds is how much speech the detector will hold before dropping
// the oldest. Segments are drained on every Feed, so this only has to cover
// one uninterrupted stretch of talking.
const bufferSeconds = 60

// Config is what the gate considers speech. The defaults are deliberately
// conservative: a false "yes" starts a recording nobody asked for, which is
// the failure mode that matters here.
type Config struct {
	// Threshold is silero's speech probability cutoff, 0..1.
	Threshold float32
	// MinSilence is how long a pause has to be before a stretch of speech is
	// considered finished.
	MinSilence float32
	// MinSpeech is how long someone has to talk before it counts at all. A
	// cough, a door and a single keystroke all clear the probability
	// threshold for a frame or two; none of them clear this.
	MinSpeech float32
}

// DefaultConfig is what the app runs with unless the settings say otherwise.
func DefaultConfig() Config {
	return Config{Threshold: 0.6, MinSilence: 0.8, MinSpeech: 0.5}
}

// Segment is one stretch of speech, as samples and as the offset it started
// at within everything ever fed to this gate.
type Segment struct {
	// StartSample counts from the first sample ever fed in, so a caller that
	// is also buffering audio can line the two up.
	StartSample int64
	Samples     []float32
}

// Seconds is how long the segment is.
func (s Segment) Seconds() float64 { return float64(len(s.Samples)) / audio.SampleRate }

// Gate is a streaming voice-activity detector. Not safe for concurrent use:
// one audio callback owns it.
type Gate struct {
	impl *sherpa.VoiceActivityDetector
	// pending holds samples that did not fill a whole window. sherpa's VAD
	// wants exact window-sized pushes, and an audio callback's chunk size
	// has nothing to do with the model's frame size.
	pending []float32
	fed     int64
}

// New loads the VAD model from modelDir (as laid out by asr.ModelDir and
// asr.Download).
func New(modelDir string, cfg Config) (*Gate, error) {
	c := sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model:              filepath.Join(modelDir, Spec.Files[0].Filename),
			Threshold:          cfg.Threshold,
			MinSilenceDuration: cfg.MinSilence,
			MinSpeechDuration:  cfg.MinSpeech,
			WindowSize:         windowSize,
			// 0 means "no cap": a long stretch of talking is exactly what
			// this gate is looking for, and chopping it up here would only
			// make the caller stitch it back together.
			MaxSpeechDuration: 0,
		},
		SampleRate: audio.SampleRate,
		NumThreads: 1,
		Provider:   "cpu",
	}
	impl := sherpa.NewVoiceActivityDetector(&c, bufferSeconds)
	// Same nil-means-rejected contract as the recognizers: sherpa-onnx logs
	// the real reason to stderr and hands back nil.
	if impl == nil {
		return nil, fmt.Errorf("vad: could not load the model from %s", modelDir)
	}
	return &Gate{impl: impl}, nil
}

// Feed pushes one audio chunk through the detector and returns whatever
// stretches of speech finished inside it. Usually none: speech ends when the
// pause after it is long enough, so a segment comes out some way into the
// silence that followed it.
func (g *Gate) Feed(chunk []float32) []Segment {
	g.pending = append(g.pending, chunk...)

	for len(g.pending) >= windowSize {
		window := g.pending[:windowSize]
		g.impl.AcceptWaveform(window)
		g.pending = g.pending[windowSize:]
		g.fed += windowSize
	}

	var out []Segment
	for !g.impl.IsEmpty() {
		seg := g.impl.Front()
		g.impl.Pop()
		if seg == nil || len(seg.Samples) == 0 {
			continue
		}
		out = append(out, Segment{StartSample: int64(seg.Start), Samples: seg.Samples})
	}
	return out
}

// Flush ends the stretch in progress and hands it back, if there is one.
//
// A segment is normally emitted only once the pause after it is long enough,
// which is the right behaviour live and the wrong one at the end of a
// finished recording: the last thing said has no pause after it, only the
// end of the file, and without this it would never come out.
func (g *Gate) Flush() []Segment {
	if len(g.pending) > 0 {
		// A partial window is still audio. Pad it out rather than drop it.
		padded := make([]float32, windowSize)
		copy(padded, g.pending)
		g.impl.AcceptWaveform(padded)
		g.fed += int64(len(g.pending))
		g.pending = nil
	}
	g.impl.Flush()

	var out []Segment
	for !g.impl.IsEmpty() {
		seg := g.impl.Front()
		g.impl.Pop()
		if seg == nil || len(seg.Samples) == 0 {
			continue
		}
		out = append(out, Segment{StartSample: int64(seg.Start), Samples: seg.Samples})
	}
	return out
}

// Speaking reports whether the detector currently believes someone is
// mid-sentence. Used for "has this gone quiet for long enough to stop",
// where waiting for the segment to be emitted would be waiting for the
// answer to a question already asked.
func (g *Gate) Speaking() bool { return g.impl.IsSpeech() }

// Reset forgets everything heard so far. Called when the gate stops being
// the thing listening (a recording took over the microphone), so the next
// arming does not emit a segment from before it.
func (g *Gate) Reset() {
	g.impl.Reset()
	g.pending = nil
}

// Close releases the model.
func (g *Gate) Close() {
	if g.impl != nil {
		sherpa.DeleteVoiceActivityDetector(g.impl)
		g.impl = nil
	}
}
