package audio

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// WAV reading and writing, for recordings too long to keep in memory.
//
// A meeting runs for an hour, and an hour of 16 kHz float32 is ~230 MB
// resident that is lost entirely if the app dies mid-call. Samples go to disk
// as they arrive instead, as 16-bit PCM (half the size, and the only format
// every other tool on the machine will open), and the decoder streams them
// back a block at a time so nothing ever holds the whole recording.

const (
	wavHeaderSize = 44
	// bytesPerSample is what the writer emits: signed 16-bit, mono. Float32
	// would round-trip exactly but doubles the file for a difference no
	// recognizer can hear -- the models normalize their own features.
	bytesPerSample = 2
)

// WAVWriter appends mono samples to a WAV file at SampleRate. The header is
// written up front with zero lengths and patched by Close, which is what makes
// streaming possible at all: the sizes cannot be known until the recording
// ends.
type WAVWriter struct {
	f *os.File
	w *bufio.Writer
	n int64 // samples written so far
}

// NewWAVWriter creates (or truncates) path and writes the header placeholder.
func NewWAVWriter(path string) (*WAVWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &WAVWriter{f: f, w: bufio.NewWriterSize(f, 64*1024)}
	if err := w.writeHeader(0); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return w, nil
}

// writeHeader emits the 44-byte canonical PCM header for n samples.
func (w *WAVWriter) writeHeader(n int64) error {
	dataBytes := uint32(n * bytesPerSample)
	var h [wavHeaderSize]byte
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+dataBytes)
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(h[20:], 1)  // PCM, uncompressed
	binary.LittleEndian.PutUint16(h[22:], 1)  // mono
	binary.LittleEndian.PutUint32(h[24:], SampleRate)
	binary.LittleEndian.PutUint32(h[28:], SampleRate*bytesPerSample) // byte rate
	binary.LittleEndian.PutUint16(h[32:], bytesPerSample)            // block align
	binary.LittleEndian.PutUint16(h[34:], 8*bytesPerSample)          // bits per sample
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], dataBytes)
	_, err := w.w.Write(h[:])
	return err
}

// Write appends samples, clipped to [-1, 1]. Callable from the audio callback:
// it only fills a buffered writer.
func (w *WAVWriter) Write(samples []float32) error {
	buf := make([]byte, len(samples)*bytesPerSample)
	for i, s := range samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		binary.LittleEndian.PutUint16(buf[i*bytesPerSample:], uint16(int16(s*32767)))
	}
	if _, err := w.w.Write(buf); err != nil {
		return err
	}
	w.n += int64(len(samples))
	return nil
}

// Samples is how many have been written so far.
func (w *WAVWriter) Samples() int64 { return w.n }

// Close flushes and patches the two length fields in the header. A file whose
// Close never ran is still readable by anything that trusts the data chunk's
// length -- it just claims to be empty, which is why Close matters.
func (w *WAVWriter) Close() error {
	if err := w.w.Flush(); err != nil {
		w.f.Close()
		return err
	}
	dataBytes := uint32(w.n * bytesPerSample)
	var sizes [4]byte

	binary.LittleEndian.PutUint32(sizes[:], 36+dataBytes)
	if _, err := w.f.WriteAt(sizes[:], 4); err != nil {
		w.f.Close()
		return err
	}
	binary.LittleEndian.PutUint32(sizes[:], dataBytes)
	if _, err := w.f.WriteAt(sizes[:], 40); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// WAVReader streams a mono 16-bit WAV back as float32 blocks.
type WAVReader struct {
	f         *os.File
	r         *bufio.Reader
	remaining int64
	total     int64
}

// OpenWAV opens path and positions the reader at the first sample. Only the
// shape this package writes is accepted -- mono 16-bit PCM -- so a file that
// is something else fails here rather than decoding into noise.
func OpenWAV(path string) (*WAVReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 64*1024)

	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: not a WAV file: %w", path, err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		f.Close()
		return nil, fmt.Errorf("%s: not a WAV file", path)
	}

	// Walk the chunk list rather than assuming a 44-byte header: files written
	// by other tools carry LIST/fact chunks before the data, and skipping a
	// fixed offset would read those bytes as audio.
	for {
		var head [8]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: no data chunk: %w", path, err)
		}
		id := string(head[0:4])
		size := int64(binary.LittleEndian.Uint32(head[4:8]))

		switch id {
		case "fmt ":
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil {
				f.Close()
				return nil, err
			}
			if len(body) < 16 {
				f.Close()
				return nil, fmt.Errorf("%s: truncated fmt chunk", path)
			}
			channels := binary.LittleEndian.Uint16(body[2:4])
			bits := binary.LittleEndian.Uint16(body[14:16])
			if channels != 1 || bits != 8*bytesPerSample {
				f.Close()
				return nil, fmt.Errorf("%s: want mono 16-bit, got %d channel(s) at %d-bit", path, channels, bits)
			}
		case "data":
			n := size / bytesPerSample
			return &WAVReader{f: f, r: r, remaining: n, total: n}, nil
		default:
			if _, err := r.Discard(int(size + size%2)); err != nil { // chunks are word-aligned
				f.Close()
				return nil, err
			}
		}
	}
}

// Samples is the recording's total length in samples.
func (r *WAVReader) Samples() int64 { return r.total }

// Seconds is the recording's length.
func (r *WAVReader) Seconds() float64 { return float64(r.total) / SampleRate }

// Read returns up to max samples, or nil once the recording is exhausted. A
// short read is not an error: the last block of a file is whatever is left.
func (r *WAVReader) Read(max int) ([]float32, error) {
	if r.remaining <= 0 || max <= 0 {
		return nil, nil
	}
	n := int64(max)
	if n > r.remaining {
		n = r.remaining
	}
	buf := make([]byte, n*bytesPerSample)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return nil, err
	}
	r.remaining -= n

	out := make([]float32, n)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(buf[i*bytesPerSample:]))) / 32767
	}
	return out, nil
}

func (r *WAVReader) Close() error { return r.f.Close() }

// ReadWAV loads a whole file. Only for recordings known to be short -- a
// meeting is read through WAVReader a block at a time instead.
func ReadWAV(path string) ([]float32, error) {
	r, err := OpenWAV(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Read(int(r.Samples()))
}
