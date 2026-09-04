package audio

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeWAV(t *testing.T, samples []float32) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "take.wav")
	w, err := NewWAVWriter(path)
	if err != nil {
		t.Fatalf("NewWAVWriter: %v", err)
	}
	if err := w.Write(samples); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestWAVRoundTrip(t *testing.T) {
	in := []float32{0, 0.5, -0.5, 0.999, -0.999}
	got, err := ReadWAV(writeWAV(t, in))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d samples, want %d", len(got), len(in))
	}
	for i := range in {
		// 16-bit quantization: one step is 1/32767, so anything under that is
		// the format, not a bug.
		if math.Abs(float64(got[i]-in[i])) > 1e-4 {
			t.Fatalf("sample %d: got %v, want %v", i, got[i], in[i])
		}
	}
}

func TestWAVClipsOutOfRangeSamples(t *testing.T) {
	// Gain above unity produces samples past full scale. Wrapping them into
	// the opposite sign would turn a loud moment into a burst of noise.
	got, err := ReadWAV(writeWAV(t, []float32{2, -2}))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if got[0] < 0.99 || got[1] > -0.99 {
		t.Fatalf("got %v, want clipped to the ends of the scale", got)
	}
}

func TestWAVHeaderLengthsArePatchedOnClose(t *testing.T) {
	// The header is written before a single sample exists, so a reader that
	// trusts it -- which is every reader -- sees an empty file unless Close
	// goes back and fixes the two length fields.
	path := writeWAV(t, make([]float32, 1000))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(wavHeaderSize + 1000*bytesPerSample); info.Size() != want {
		t.Fatalf("file is %d bytes, want %d", info.Size(), want)
	}

	r, err := OpenWAV(path)
	if err != nil {
		t.Fatalf("OpenWAV: %v", err)
	}
	defer r.Close()
	if r.Samples() != 1000 {
		t.Fatalf("header claims %d samples, want 1000", r.Samples())
	}
}

func TestWAVReaderYieldsBlocks(t *testing.T) {
	in := make([]float32, 2500)
	for i := range in {
		in[i] = float32(i%100) / 200
	}
	r, err := OpenWAV(writeWAV(t, in))
	if err != nil {
		t.Fatalf("OpenWAV: %v", err)
	}
	defer r.Close()

	var blocks int
	var total int
	for {
		block, err := r.Read(1000)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if block == nil {
			break
		}
		blocks++
		total += len(block)
	}
	if blocks != 3 || total != 2500 {
		t.Fatalf("got %d blocks totalling %d samples, want 3 and 2500", blocks, total)
	}
}

func TestWAVReaderReportsLength(t *testing.T) {
	r, err := OpenWAV(writeWAV(t, make([]float32, SampleRate*2)))
	if err != nil {
		t.Fatalf("OpenWAV: %v", err)
	}
	defer r.Close()
	if math.Abs(r.Seconds()-2) > 1e-9 {
		t.Fatalf("got %v seconds, want 2", r.Seconds())
	}
}

func TestWAVWriterStreamsAcrossManyWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meeting.wav")
	w, err := NewWAVWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := w.Write(make([]float32, 160)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if w.Samples() != 16000 {
		t.Fatalf("writer counted %d samples, want 16000", w.Samples())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ReadWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 16000 {
		t.Fatalf("read back %d samples, want 16000", len(got))
	}
}

func TestOpenWAVRejectsSomethingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notaudio.wav")
	if err := os.WriteFile(path, []byte("this is not a wav file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWAV(path); err == nil {
		t.Fatal("OpenWAV accepted a non-WAV file; it would decode as noise")
	}
}

func TestOpenWAVSkipsUnknownChunks(t *testing.T) {
	// Files written by other tools carry LIST/fact chunks before the audio.
	// Assuming a 44-byte header would read those bytes as samples.
	path := filepath.Join(t.TempDir(), "extra.wav")
	src := writeWAV(t, []float32{0.5, -0.5})
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	// Splice a 4-byte LIST chunk in between the fmt and data chunks.
	var out []byte
	out = append(out, raw[:36]...)
	out = append(out, 'L', 'I', 'S', 'T', 4, 0, 0, 0, 'I', 'N', 'F', 'O')
	out = append(out, raw[36:]...)
	// RIFF size grows by the 12 bytes just inserted.
	out[4] += 12
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadWAV(path)
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if len(got) != 2 || got[0] < 0.4 {
		t.Fatalf("got %v, want the two samples past the LIST chunk", got)
	}
}

func TestReadRangeCutsOutTheMiddle(t *testing.T) {
	samples := make([]float32, 5*SampleRate)
	for i := range samples {
		samples[i] = float32(i%1000) / 1000
	}
	path := filepath.Join(t.TempDir(), "take.wav")
	w, err := NewWAVWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(samples); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ReadRange(path, 2, 4)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(got) != 2*SampleRate {
		t.Fatalf("got %d samples, want %d", len(got), 2*SampleRate)
	}
	want := samples[2*SampleRate]
	if diff := got[0] - want; diff > 1e-3 || diff < -1e-3 {
		t.Fatalf("range starts at %v, want %v -- the skip landed in the wrong place", got[0], want)
	}

	// Past the end of the recording, and backwards: both are things a stale
	// turn timing can ask for, and neither is an error.
	if got, err := ReadRange(path, 60, 62); err != nil || got != nil {
		t.Fatalf("past the end: got %d samples, %v", len(got), err)
	}
	if got, err := ReadRange(path, 3, 1); err != nil || got != nil {
		t.Fatalf("backwards: got %d samples, %v", len(got), err)
	}
}
