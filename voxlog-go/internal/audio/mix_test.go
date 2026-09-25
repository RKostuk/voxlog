package audio

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeWAVAt(t *testing.T, dir, name string, samples []float32) string {
	t.Helper()
	path := filepath.Join(dir, name)
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

// decodeMix reads a MixReader whole and returns the samples it carried.
func decodeMix(t *testing.T, m *MixReader) []float32 {
	t.Helper()
	all, err := io.ReadAll(m)
	if err != nil {
		t.Fatalf("read mix: %v", err)
	}
	if len(all) < wavHeaderSize {
		t.Fatalf("mix is %d bytes, shorter than a WAV header", len(all))
	}
	if string(all[0:4]) != "RIFF" || string(all[8:12]) != "WAVE" {
		t.Fatalf("mix does not start with a WAV header")
	}
	body := all[wavHeaderSize:]
	out := make([]float32, len(body)/bytesPerSample)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(body[i*bytesPerSample:]))) / 32767
	}
	return out
}

func TestMixSumsBothTracks(t *testing.T) {
	dir := t.TempDir()
	mic := writeWAVAt(t, dir, "call-mic.wav", []float32{0.25, 0, -0.25, 0})
	sys := writeWAVAt(t, dir, "call-system.wav", []float32{0, 0.5, 0, -0.5})

	m, err := OpenMix(mic, sys)
	if err != nil {
		t.Fatalf("OpenMix: %v", err)
	}
	defer m.Close()

	got := decodeMix(t, m)
	want := []float32{0.25, 0.5, -0.25, -0.5}
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-3 {
			t.Fatalf("sample %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMixRunsForTheLongerTrack(t *testing.T) {
	// The two files are never exactly the same length -- the system tap
	// starts a moment after the microphone -- and past the end of the shorter
	// one the answer is silence, not a truncated meeting.
	dir := t.TempDir()
	mic := writeWAVAt(t, dir, "call-mic.wav", []float32{0.5, 0.5, 0.5, 0.5, 0.5})
	sys := writeWAVAt(t, dir, "call-system.wav", []float32{0.25})

	m, err := OpenMix(mic, sys)
	if err != nil {
		t.Fatalf("OpenMix: %v", err)
	}
	defer m.Close()

	got := decodeMix(t, m)
	if len(got) != 5 {
		t.Fatalf("got %d samples, want 5", len(got))
	}
	if math.Abs(float64(got[0]-0.75)) > 1e-3 {
		t.Fatalf("first sample: got %v, want 0.75", got[0])
	}
	if math.Abs(float64(got[4]-0.5)) > 1e-3 {
		t.Fatalf("last sample: got %v, want 0.5 (mic alone)", got[4])
	}
}

func TestMixClipsRatherThanWraps(t *testing.T) {
	dir := t.TempDir()
	mic := writeWAVAt(t, dir, "call-mic.wav", []float32{0.9, -0.9})
	sys := writeWAVAt(t, dir, "call-system.wav", []float32{0.9, -0.9})

	m, err := OpenMix(mic, sys)
	if err != nil {
		t.Fatalf("OpenMix: %v", err)
	}
	defer m.Close()

	got := decodeMix(t, m)
	if got[0] < 0.99 || got[1] > -0.99 {
		t.Fatalf("two loud tracks wrapped instead of clipping: %v", got)
	}
}

func TestMixWithoutACallTrackIsTheMicrophone(t *testing.T) {
	dir := t.TempDir()
	mic := writeWAVAt(t, dir, "call-mic.wav", []float32{0.5, -0.5})

	m, err := OpenMix(mic, filepath.Join(dir, "call-system.wav"))
	if err != nil {
		t.Fatalf("OpenMix: %v", err)
	}
	defer m.Close()

	got := decodeMix(t, m)
	if len(got) != 2 || math.Abs(float64(got[0]-0.5)) > 1e-3 {
		t.Fatalf("mic-only meeting did not play: %v", got)
	}
}

func TestMixSeeksIntoTheMiddle(t *testing.T) {
	// What a range request is: the window's timeline jumps into an hour-long
	// call, and the player asks for the bytes from there.
	dir := t.TempDir()
	mic := writeWAVAt(t, dir, "call-mic.wav", []float32{0, 0, 0.5, 0.5})
	sys := writeWAVAt(t, dir, "call-system.wav", []float32{0, 0, 0.25, 0.25})

	m, err := OpenMix(mic, sys)
	if err != nil {
		t.Fatalf("OpenMix: %v", err)
	}
	defer m.Close()

	if _, err := m.Seek(wavHeaderSize+2*bytesPerSample, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	rest, err := io.ReadAll(m)
	if err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if len(rest) != 2*bytesPerSample {
		t.Fatalf("got %d bytes after seeking to sample 2, want %d", len(rest), 2*bytesPerSample)
	}
	first := float64(int16(binary.LittleEndian.Uint16(rest))) / 32767
	if math.Abs(first-0.75) > 1e-3 {
		t.Fatalf("first sample after the seek: got %v, want 0.75", first)
	}

	// Seeking to the end is what http.ServeContent does to find the size.
	end, err := m.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek end: %v", err)
	}
	if end != m.Size() {
		t.Fatalf("SeekEnd gave %d, want the size %d", end, m.Size())
	}
}

func TestMixSeeksToAnOddByte(t *testing.T) {
	// A player is free to ask for a range starting halfway through a sample.
	dir := t.TempDir()
	mic := writeWAVAt(t, dir, "call-mic.wav", []float32{0.5, 0.5})
	sys := writeWAVAt(t, dir, "call-system.wav", []float32{0.25, 0.25})

	m, err := OpenMix(mic, sys)
	if err != nil {
		t.Fatalf("OpenMix: %v", err)
	}
	defer m.Close()

	whole, err := io.ReadAll(m)
	if err != nil {
		t.Fatalf("read whole: %v", err)
	}
	if _, err := m.Seek(wavHeaderSize+1, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	odd, err := io.ReadAll(m)
	if err != nil {
		t.Fatalf("read after odd seek: %v", err)
	}
	if !bytes.Equal(odd, whole[wavHeaderSize+1:]) {
		t.Fatalf("an odd-byte range did not line up with the whole stream")
	}
}

func TestMixNeedsTheMicrophoneFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenMix(filepath.Join(dir, "gone-mic.wav"), ""); err == nil {
		t.Fatal("OpenMix accepted a recording that is not on disk")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone-mic.wav")); !os.IsNotExist(err) {
		t.Fatal("the test wrote a file it should not have")
	}
}
