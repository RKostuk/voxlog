package main

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/diarize"
)

// Runs the whole call-diarization path over real speech, against whatever
// models are actually downloaded: diarize, slice, decode each turn. It is
// skipped when the models are absent, so it costs nothing on a fresh
// checkout -- but on a machine that has them it is the only test that
// exercises two ONNX models loaded at once, which is where a crash was first
// reported.
func TestDiarizationPipelineOverRealSpeech(t *testing.T) {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Library", "Application Support", "Voxlog", "models")

	var spec asr.ModelSpec
	for _, m := range knownModels {
		if m.Family == "parakeet" {
			spec = m
		}
	}
	if !asr.IsDownloaded(base, spec) || !asr.IsDownloaded(base, diarize.Spec) {
		t.Skip("models not downloaded")
	}

	// Real speech from two different macOS voices, alternating, so the
	// segmentation model has something it can actually cluster.
	a := readWAV(t, sayToFile(t, "Samantha", "Hello, this is the first speaker talking about the quarterly report and the deadline on Friday."))
	b := readWAV(t, sayToFile(t, "Daniel", "And this is a completely different person answering with another voice and a different accent entirely."))
	var samples []float32
	for turn := 0; turn < 3; turn++ {
		samples = append(samples, a...)
		samples = append(samples, make([]float32, audio.SampleRate/2)...)
		samples = append(samples, b...)
		samples = append(samples, make([]float32, audio.SampleRate/2)...)
	}
	t.Logf("input: %.1fs", float64(len(samples))/audio.SampleRate)

	t.Log("loading diarizer")
	d, err := diarize.New(asr.ModelDir(base, diarize.Spec), 4)
	if err != nil {
		t.Fatalf("diarize.New: %v", err)
	}
	defer d.Close()

	t.Log("processing")
	segments := diarize.Merge(d.Process(samples), maxSpeakerGap, minSpeakerSegment)
	t.Logf("segments: %d speakers=%d", len(segments), countSpeakers(segments))

	t.Log("loading parakeet")
	tr, err := asr.NewTranscriber(spec, asr.ModelDir(base, spec), "auto")
	if err != nil {
		t.Fatalf("NewTranscriber: %v", err)
	}
	defer tr.Close()

	for i, s := range segments {
		slice := diarize.Slice(samples, s, audio.SampleRate)
		t.Logf("segment %d: %.2f-%.2f spk=%d samples=%d", i, s.Start, s.End, s.Speaker, len(slice))
		out, err := tr.Transcribe(slice, "auto")
		if err != nil {
			t.Fatalf("segment %d transcribe: %v", i, err)
		}
		t.Logf("  -> %q", out)
	}
}

// readWAV pulls float32 PCM out of the data chunk of a WAVE file.
func readWAV(t *testing.T, path string) []float32 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no sample audio: %v", err)
	}
	i := 12 // past "RIFF....WAVE"
	for i+8 <= len(raw) {
		id := string(raw[i : i+4])
		size := int(binary.LittleEndian.Uint32(raw[i+4 : i+8]))
		body := i + 8
		if id == "data" {
			end := body + size
			if end > len(raw) {
				end = len(raw)
			}
			out := make([]float32, 0, (end-body)/4)
			for j := body; j+4 <= end; j += 4 {
				out = append(out, math.Float32frombits(binary.LittleEndian.Uint32(raw[j:j+4])))
			}
			return out
		}
		i = body + size
	}
	t.Fatalf("no data chunk in %s", path)
	return nil
}

// sayToFile renders text with a macOS system voice at the recognizer's own
// sample rate, so the test carries no audio fixtures of its own.
func sayToFile(t *testing.T, voice, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), voice+".wav")
	cmd := exec.Command("say", "-v", voice, "-o", path,
		"--data-format=LEF32@16000", "--file-format=WAVE", text)
	if err := cmd.Run(); err != nil {
		t.Skipf("say -v %s unavailable: %v", voice, err)
	}
	return path
}

// The word-labelling path over the same real speech: one decode of the whole
// channel, speakers attached by timestamp. Logs both transcripts so the two
// can be compared by eye -- the per-segment one above is what produced junk.
func TestWordLabelledDiarizationOverRealSpeech(t *testing.T) {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Library", "Application Support", "Voxlog", "models")

	var spec asr.ModelSpec
	for _, m := range knownModels {
		if m.Family == "parakeet" {
			spec = m
		}
	}
	if !asr.IsDownloaded(base, spec) || !asr.IsDownloaded(base, diarize.Spec) {
		t.Skip("models not downloaded")
	}

	a := readWAV(t, sayToFile(t, "Samantha", "Hello, this is the first speaker talking about the quarterly report and the deadline on Friday."))
	b := readWAV(t, sayToFile(t, "Daniel", "And this is a completely different person answering with another voice and a different accent entirely."))
	var samples []float32
	for turn := 0; turn < 3; turn++ {
		samples = append(samples, a...)
		samples = append(samples, make([]float32, audio.SampleRate/2)...)
		samples = append(samples, b...)
		samples = append(samples, make([]float32, audio.SampleRate/2)...)
	}

	d, err := diarize.New(asr.ModelDir(base, diarize.Spec), 4)
	if err != nil {
		t.Fatalf("diarize.New: %v", err)
	}
	defer d.Close()
	segments := diarize.Merge(d.Process(samples), maxSpeakerGap, minSpeakerSegment)

	tr, err := asr.NewTranscriber(spec, asr.ModelDir(base, spec), "auto")
	if err != nil {
		t.Fatalf("NewTranscriber: %v", err)
	}
	defer tr.Close()

	wt, ok := tr.(asr.WordTranscriber)
	if !ok {
		t.Fatal("parakeet does not implement WordTranscriber, so the labelling path is dead")
	}
	words, err := wt.TranscribeWords(samples, "auto")
	if err != nil {
		t.Fatalf("TranscribeWords: %v", err)
	}
	if len(words) == 0 {
		t.Fatal("no words came back with timings")
	}
	t.Logf("words: %d, first %+v, last %+v", len(words), words[0], words[len(words)-1])

	out := renderTurns(wordTurns(words, segments, 0, true), nil)
	t.Logf("transcript:\n%s", out)
	if out == "" {
		t.Fatal("labelled transcript is empty")
	}
	if countSpeakers(segments) < 2 {
		t.Errorf("only %d speaker found in alternating speech", countSpeakers(segments))
	}
}
