package audio

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// A meeting is recorded as two files -- the microphone and the call -- and
// never mixed on disk: separating the two is what lets the decoder tell the
// user apart from everybody else, and a third file would be half again as
// much disk for audio nothing decodes.
//
// Playing the meeting back, though, is the opposite problem: a player handed
// one of the two files plays half a conversation. MixReader is the join,
// made at read time. It presents the two files as one WAV stream, sample for
// sample, and it seeks: the window's timeline jumps an hour into a call with
// a range request rather than a download, which is the only reason this is a
// reader rather than a file written once at the end of the recording.
type MixReader struct {
	mic, sys *pcm
	samples  int64 // the longer of the two; the shorter is silence past its end
	header   []byte
	size     int64
	off      int64
	scratch  []byte
}

// pcm is one open recording: the file, where its samples start, and how many
// there are. No buffered reader -- every read here is a ReadAt at a computed
// offset, because the caller is seeking.
type pcm struct {
	f     *os.File
	start int64
	n     int64
}

// OpenMix opens both halves of a meeting. The system path may be empty or
// missing -- a meeting recorded with no far end, or one whose call track was
// swept -- and then the mix is simply the microphone.
func OpenMix(micPath, sysPath string) (*MixReader, error) {
	mic, err := openPCM(micPath)
	if err != nil {
		return nil, err
	}
	var sys *pcm
	if sysPath != "" {
		// A missing call track is not an error: play what there is.
		if s, err := openPCM(sysPath); err == nil {
			sys = s
		}
	}

	samples := mic.n
	if sys != nil && sys.n > samples {
		samples = sys.n
	}
	m := &MixReader{
		mic:     mic,
		sys:     sys,
		samples: samples,
		header:  wavHeader(uint32(samples * bytesPerSample)),
	}
	m.size = wavHeaderSize + samples*bytesPerSample
	return m, nil
}

// openPCM parses a WAV header the same way OpenWAV does -- the data chunk is
// not at a fixed offset -- and keeps the file for random access.
func openPCM(path string) (*pcm, error) {
	r, err := OpenWAV(path)
	if err != nil {
		return nil, err
	}
	// The buffered reader is thrown away; the file underneath it is what
	// ReadAt needs, and it is closed through the pcm.
	return &pcm{f: r.f, start: r.dataStart, n: r.total}, nil
}

// Size is the length of the mixed stream in bytes, header included.
func (m *MixReader) Size() int64 { return m.size }

// Seconds is how long the mixed recording plays for.
func (m *MixReader) Seconds() float64 { return float64(m.samples) / SampleRate }

func (m *MixReader) Seek(offset int64, whence int) (int64, error) {
	var at int64
	switch whence {
	case io.SeekStart:
		at = offset
	case io.SeekCurrent:
		at = m.off + offset
	case io.SeekEnd:
		at = m.size + offset
	default:
		return 0, errors.New("audio: bad whence")
	}
	if at < 0 {
		return 0, errors.New("audio: seek before start")
	}
	m.off = at
	return at, nil
}

func (m *MixReader) Read(p []byte) (int, error) {
	if m.off >= m.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if left := m.size - m.off; int64(len(p)) > left {
		p = p[:left]
	}

	// The header, for a reader that started at the beginning.
	if m.off < wavHeaderSize {
		n := copy(p, m.header[m.off:])
		m.off += int64(n)
		return n, nil
	}

	// A range request can land mid-sample, so the mix is made a whole sample
	// at a time and the odd leading byte is dropped from the copy.
	dataOff := m.off - wavHeaderSize
	skew := int(dataOff % bytesPerSample)
	first := dataOff / bytesPerSample
	want := (len(p) + skew + bytesPerSample - 1) / bytesPerSample
	if int64(want) > m.samples-first {
		want = int(m.samples - first)
	}
	if want <= 0 {
		return 0, io.EOF
	}

	need := want * bytesPerSample
	if cap(m.scratch) < need {
		m.scratch = make([]byte, need)
	}
	buf := m.scratch[:need]
	readInto(buf, m.mic, first)
	if m.sys != nil {
		mixInto(buf, m.sys, first)
	}

	n := copy(p, buf[skew:])
	m.off += int64(n)
	return n, nil
}

// readInto fills buf with want samples of one recording starting at sample
// first, zero-filling whatever the file does not reach -- the two tracks are
// rarely the same length, and past the end of the shorter one the answer is
// silence, not an error.
func readInto(buf []byte, src *pcm, first int64) {
	for i := range buf {
		buf[i] = 0
	}
	if first >= src.n {
		return
	}
	have := (src.n - first) * bytesPerSample
	if have > int64(len(buf)) {
		have = int64(len(buf))
	}
	n, err := src.f.ReadAt(buf[:have], src.start+first*bytesPerSample)
	if err != nil && n < int(have) {
		// A short read is silence for the rest, same as running off the end.
		for i := n; i < len(buf); i++ {
			buf[i] = 0
		}
	}
}

// mixInto adds a second recording into buf, sample by sample, clipping at
// full scale. The same summing rule the decoder's own mixAudio uses: two
// people talking at once is louder, not quieter, and the alternative --
// halving both -- makes every single-speaker stretch quiet for the sake of
// the rare overlap.
func mixInto(buf []byte, src *pcm, first int64) {
	if first >= src.n {
		return
	}
	other := make([]byte, len(buf))
	readInto(other, src, first)
	for i := 0; i+1 < len(buf); i += bytesPerSample {
		sum := int32(int16(binary.LittleEndian.Uint16(buf[i:]))) +
			int32(int16(binary.LittleEndian.Uint16(other[i:])))
		if sum > 32767 {
			sum = 32767
		} else if sum < -32768 {
			sum = -32768
		}
		binary.LittleEndian.PutUint16(buf[i:], uint16(int16(sum)))
	}
}

// Close releases both files.
func (m *MixReader) Close() error {
	err := m.mic.f.Close()
	if m.sys != nil {
		if e := m.sys.f.Close(); err == nil {
			err = e
		}
	}
	return err
}
