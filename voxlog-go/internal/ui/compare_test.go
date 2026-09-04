package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"voxlog-go/internal/asr"
)

func TestModelComparisonEndToEnd(t *testing.T) {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Library", "Application Support", "Voxlog", "models")

	specs := []asr.ModelSpec{
		{Family: "parakeet", Variant: "tdt-0.6b-v3", Files: []asr.ModelFile{
			{Filename: "encoder.onnx"}, {Filename: "encoder.weights"},
			{Filename: "decoder.onnx"}, {Filename: "joiner.onnx"}, {Filename: "tokens.txt"}}},
		{Family: "nemotron", Variant: "streaming-320ms", Files: []asr.ModelFile{
			{Filename: "encoder.onnx"}, {Filename: "decoder.onnx"},
			{Filename: "joiner.onnx"}, {Filename: "tokens.txt"}}},
	}

	downloaded := 0
	for _, spec := range specs {
		if asr.IsDownloaded(base, spec) {
			downloaded++
		}
	}
	if downloaded < 2 {
		t.Skip("needs two downloaded models to compare")
	}

	var c modelComparison
	if err := c.start("", 1.0); err != nil {
		t.Skipf("no microphone: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	rows, err := c.finish(specs, base, "uk")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	for _, r := range rows {
		t.Logf("%v %v: text=%q load=%.1fs decode=%.2fs err=%v",
			r["family"], r["variant"], r["text"], r["load_seconds"], r["seconds"], r["error"])
	}
	if len(rows) != downloaded {
		t.Fatalf("got %d rows, want one per downloaded model (%d)", len(rows), downloaded)
	}
	for _, r := range rows {
		if r["error"] == nil {
			if _, ok := r["seconds"]; !ok {
				t.Errorf("%v reported no decode time", r["family"])
			}
		}
	}
}
