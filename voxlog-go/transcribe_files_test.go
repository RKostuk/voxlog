package main

import (
	"os"
	"path/filepath"
	"testing"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/settings"
)

// The whole meeting path end to end, over real speech and the model actually
// installed: a WAV on disk goes in, turns with absolute timings come out.
//
// This is the test that would have caught a decode that produces nothing --
// the unit tests around it all use fake audio and never load a recognizer.
// Skipped when no ASR model is downloaded, like the other model-gated tests.
func TestTranscribeFilesTurnsOverRealSpeech(t *testing.T) {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Library", "Application Support", "Voxlog", "models")

	var spec asr.ModelSpec
	for _, m := range knownModels {
		if m.Family == "parakeet" {
			spec = m
		}
	}
	if !asr.IsDownloaded(base, spec) {
		t.Skip("models not downloaded")
	}

	// say writes 32-bit float WAVs; the app only ever reads back what it
	// wrote itself, which is 16-bit mono -- so the fixture goes through the
	// app's own writer, exactly as a real recording does.
	samples := readWAV(t, sayToFile(t, "Samantha",
		"This is a test of the meeting transcription path, with a sentence long enough to decode."))
	path := filepath.Join(t.TempDir(), "meeting-mic.wav")
	w, err := audio.NewWAVWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(samples); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	a := &app{
		store:     settings.NewStore(filepath.Join(t.TempDir(), "settings.json")),
		models:    &transcriberCache{},
		speakers:  &diarizerCache{},
		embedders: &embedderCache{},
		modelsDir: base,
	}

	turns, err := a.transcribeFilesTurns(spec, "", path, "", false, func() {}, nil)
	if err != nil {
		t.Fatalf("transcribeFilesTurns: %v", err)
	}
	if len(turns) == 0 {
		t.Fatal("no turns came back from a recording of real speech")
	}
	text := renderTurns(turns, nil)
	if text == "" {
		t.Fatal("turns rendered to an empty transcript")
	}
	t.Logf("transcript: %q", text)

	// Timings have to describe the file, not the block: a turn that ends
	// after the recording does cannot be played back.
	r, err := audio.OpenWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	length := float32(r.Seconds())
	r.Close()
	for i, tn := range turns {
		if tn.start < 0 || tn.end > length+1 || tn.end < tn.start {
			t.Fatalf("turn %d has timings outside the %0.1fs recording: %+v", i, length, tn)
		}
	}
}

// The dictation path over the same speech, for the same reason: it shares
// transcribeBlock with the meeting path, and a change to one can silently
// empty the other.
func TestTranscribeSamplesOverRealSpeech(t *testing.T) {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Library", "Application Support", "Voxlog", "models")

	var spec asr.ModelSpec
	for _, m := range knownModels {
		if m.Family == "parakeet" {
			spec = m
		}
	}
	if !asr.IsDownloaded(base, spec) {
		t.Skip("models not downloaded")
	}

	samples := readWAV(t, sayToFile(t, "Samantha", "Dictation still has to produce text."))
	a := &app{
		store:     settings.NewStore(filepath.Join(t.TempDir(), "settings.json")),
		models:    &transcriberCache{},
		speakers:  &diarizerCache{},
		embedders: &embedderCache{},
		modelsDir: base,
	}

	if text := a.transcribeSamples(spec, "", samples, nil, false, func() {}); text == "" {
		t.Fatal("dictation produced no transcript")
	} else {
		t.Logf("transcript: %q", text)
	}
}
