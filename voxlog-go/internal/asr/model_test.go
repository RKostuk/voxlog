package asr

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestModelDirIsFamilyVariant(t *testing.T) {
	m := ModelSpec{Family: "parakeet", Variant: "tdt-0.6b-v3"}
	got := ModelDir("/base", m)
	want := filepath.Join("/base", "parakeet-tdt-0.6b-v3")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestIsDownloadedFalseWhenMissing(t *testing.T) {
	base := t.TempDir()
	m := ModelSpec{Family: "whisper", Variant: "base.en", Files: []ModelFile{{Filename: "model.onnx"}}}
	if IsDownloaded(base, m) {
		t.Fatal("expected not downloaded")
	}
}

func TestDownloadThenIsDownloadedTrue(t *testing.T) {
	base := t.TempDir()
	m := ModelSpec{
		Family:  "whisper",
		Variant: "base.en",
		Files: []ModelFile{
			{URL: "https://example.invalid/model.onnx", Filename: "model.onnx"},
			{URL: "https://example.invalid/tokens.txt", Filename: "tokens.txt"},
		},
	}

	fetch := func(url string) (io.ReadCloser, int64, error) {
		body := []byte("fake-bytes-for-" + url)
		return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	}

	var events []string
	progress := func(file string, downloaded, total int64) {
		events = append(events, file)
	}

	if err := Download(base, m, progress, fetch); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !IsDownloaded(base, m) {
		t.Fatal("expected downloaded after Download()")
	}
	if len(events) == 0 {
		t.Fatal("expected progress callback to fire")
	}

	data, err := os.ReadFile(filepath.Join(ModelDir(base, m), "model.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fake-bytes-for-https://example.invalid/model.onnx" {
		t.Fatalf("got %q", data)
	}
}

func TestDownloadSkipsAlreadyPresentFiles(t *testing.T) {
	base := t.TempDir()
	m := ModelSpec{
		Family:  "whisper",
		Variant: "base.en",
		Files:   []ModelFile{{URL: "https://example.invalid/model.onnx", Filename: "model.onnx"}},
	}
	dir := ModelDir(base, m)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("already-here"), 0o644); err != nil {
		t.Fatal(err)
	}

	fetch := func(url string) (io.ReadCloser, int64, error) {
		t.Fatal("fetch should not be called for an already-present file")
		return nil, 0, nil
	}

	if err := Download(base, m, func(string, int64, int64) {}, fetch); err != nil {
		t.Fatalf("Download: %v", err)
	}
}

// failingReader returns a few bytes successfully, then a read error.
type failingReader struct {
	data []byte
	sent bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.data)
		return n, nil
	}
	return 0, errors.New("connection reset")
}

func (r *failingReader) Close() error { return nil }

func TestDownloadLeavesNoPartialFileOnReadError(t *testing.T) {
	base := t.TempDir()
	m := ModelSpec{
		Family:  "whisper",
		Variant: "base.en",
		Files:   []ModelFile{{URL: "https://example.invalid/model.onnx", Filename: "model.onnx"}},
	}

	fetch := func(url string) (io.ReadCloser, int64, error) {
		return &failingReader{data: []byte("partial")}, 1000, nil
	}

	err := Download(base, m, func(string, int64, int64) {}, fetch)
	if err == nil {
		t.Fatal("expected error from failing read")
	}

	dest := filepath.Join(ModelDir(base, m), "model.onnx")
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatalf("expected no file at %s after failed download, but one exists", dest)
	}
	if IsDownloaded(base, m) {
		t.Fatal("expected IsDownloaded to be false after failed download")
	}

	// also confirm no leftover .part file
	entries, _ := os.ReadDir(ModelDir(base, m))
	for _, e := range entries {
		t.Fatalf("unexpected leftover file after failed download: %s", e.Name())
	}
}
