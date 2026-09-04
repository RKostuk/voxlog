package asr

import (
	"path/filepath"
	"testing"
)

func TestNewTranscriberErrorsWhenModelDirMissing(t *testing.T) {
	m := ModelSpec{Family: "whisper", Variant: "base.en", Files: []ModelFile{{Filename: "model.onnx"}}}
	_, err := NewTranscriber(m, filepath.Join(t.TempDir(), "does-not-exist"), "auto")
	if err == nil {
		t.Fatal("expected error for missing model directory")
	}
}

func TestNewTranscriberUnknownFamilyErrors(t *testing.T) {
	m := ModelSpec{Family: "not-a-real-family", Variant: "x"}
	_, err := NewTranscriber(m, t.TempDir(), "auto")
	if err == nil {
		t.Fatal("expected error for unknown family")
	}
}
