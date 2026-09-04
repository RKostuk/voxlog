// Package audio captures mono float32 PCM from the microphone via malgo
// (cgo bindings to miniaudio). Port of dictate_app/audio.py's AudioRecorder.
package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// SampleRate matches sherpa-onnx's expected input rate.
const SampleRate = 16000

// Recorder captures mono float32 PCM at SampleRate from the given input
// device (empty string == system default) until Stop is called.
type Recorder struct {
	deviceName string
	gain       float64

	ctx    *malgo.AllocatedContext
	device *malgo.Device

	mu      sync.Mutex
	buf     []float32
	onChunk func([]float32)
	// subs are extra listeners on the same capture, added and dropped while
	// it runs (see Attach). This is what lets a dictation happen in the
	// middle of a meeting without opening the microphone a second time: the
	// meeting owns the device, the dictation listens in and keeps its own
	// buffer of the stretch it cares about.
	subs    map[int]func([]float32)
	nextSub int
	// unbuffered drops the recorder's own copy of the audio, for a capture
	// whose samples are being written somewhere else as they arrive.
	unbuffered bool
	// level is the loudest chunk seen SINCE THE METER LAST LOOKED, measured
	// after gain. Audio arrives every 10ms and the meters read at 25Hz, so
	// holding only the newest chunk threw away three readings out of four --
	// and with them every transient shorter than the gap, which is most
	// consonants. Holding the maximum instead means a peak cannot slip
	// between two reads, which is the whole job of a peak meter.
	level float64
}

// NewRecorder allocates a malgo context for capture. deviceName selects an
// input device by name; empty string uses the system default.
func NewRecorder(deviceName string, gain float64) (*Recorder, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	return &Recorder{deviceName: deviceName, gain: gain, ctx: ctx}, nil
}

// Start begins capture, discarding any previously buffered audio.
func (r *Recorder) Start() error {
	return r.start(nil)
}

// StartStreaming is like Start, but additionally invokes onChunk with each
// captured chunk as it arrives, for the Nemotron live-partial-text path.
func (r *Recorder) StartStreaming(onChunk func([]float32)) error {
	r.mu.Lock()
	r.unbuffered = false
	r.mu.Unlock()
	return r.start(onChunk)
}

// StartUnbuffered captures without keeping the samples in memory: onChunk gets
// every chunk and Stop returns nothing. This is what a meeting uses -- an hour
// of audio is 230MB resident, and it goes to disk as it arrives instead.
func (r *Recorder) StartUnbuffered(onChunk func([]float32)) error {
	r.mu.Lock()
	r.unbuffered = true
	r.mu.Unlock()
	return r.start(onChunk)
}

func (r *Recorder) start(onChunk func([]float32)) error {
	r.mu.Lock()
	r.buf = nil
	r.onChunk = onChunk
	r.mu.Unlock()

	deviceID, err := r.resolveDeviceID()
	if err != nil {
		return err
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = malgo.FormatF32
	deviceConfig.Capture.Channels = 1
	deviceConfig.SampleRate = SampleRate
	if deviceID != nil {
		// Pinned, not merely referenced: the ID lives in Go memory (it comes
		// out of the slice ctx.Devices returned), and InitDevice hands the
		// whole config to C. An unpinned Go pointer crossing that boundary is
		// a hard panic -- "argument of cgo function has Go pointer to
		// unpinned Go pointer" -- which is what picking any input device by
		// name did, while the system-default path (a nil ID) never noticed.
		//
		// The pin only has to outlive InitDevice: miniaudio copies the ID
		// into the device it allocates.
		var pinner runtime.Pinner
		pinner.Pin(deviceID)
		defer pinner.Unpin()
		deviceConfig.Capture.DeviceID = unsafe.Pointer(deviceID)
	}

	callbacks := malgo.DeviceCallbacks{
		Data: r.onData,
	}
	device, err := malgo.InitDevice(r.ctx.Context, deviceConfig, callbacks)
	if err != nil {
		return fmt.Errorf("init capture device: %w", err)
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		return fmt.Errorf("start capture device: %w", err)
	}
	r.device = device
	return nil
}

// resolveDeviceID looks up the capture device ID matching r.deviceName. It
// returns a nil ID (system default) if deviceName is empty or not found.
func (r *Recorder) resolveDeviceID() (*malgo.DeviceID, error) {
	if r.deviceName == "" {
		return nil, nil
	}
	infos, err := r.ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, fmt.Errorf("enumerate capture devices: %w", err)
	}
	for i := range infos {
		if infos[i].Name() == r.deviceName {
			return &infos[i].ID, nil
		}
	}
	// Fall back to system default if the named device isn't present.
	return nil, nil
}

// onData is malgo's capture data callback: converts the raw little-endian
// float32 PCM bytes to []float32, applies gain (clipped to [-1, 1]), and
// appends to the buffer.
func (r *Recorder) onData(_ []byte, pInputSamples []byte, framecount uint32) {
	n := int(framecount)
	chunk := make([]float32, n)
	var sumSquares float64
	for i := 0; i < n; i++ {
		bits := binary.LittleEndian.Uint32(pInputSamples[i*4 : i*4+4])
		s := math.Float32frombits(bits)
		if r.gain != 1.0 {
			s = float32(math.Min(1.0, math.Max(-1.0, float64(s)*r.gain)))
		}
		// After gain, and after the clip: the meter shows what actually
		// reaches the recognizer, so a slider set hot enough to clip reads as
		// pinned rather than as merely loud.
		sumSquares += float64(s) * float64(s)
		chunk[i] = s
	}

	level := 0.0
	if n > 0 {
		level = scaleLevel(math.Sqrt(sumSquares / float64(n)))
	}

	r.mu.Lock()
	// Not buffering is the exception (see StartUnbuffered), so the zero value
	// keeps every other path -- and every test -- accumulating as before.
	if !r.unbuffered {
		r.buf = append(r.buf, chunk...)
	}
	if level > r.level {
		r.level = level
	}
	onChunk := r.onChunk
	// Copied out under the lock, called outside it: a subscriber that blocks
	// must not also block Attach/detach, and this is miniaudio's realtime
	// thread either way.
	subs := make([]func([]float32), 0, len(r.subs))
	for _, fn := range r.subs {
		subs = append(subs, fn)
	}
	r.mu.Unlock()

	if onChunk != nil {
		onChunk(chunk)
	}
	for _, fn := range subs {
		fn(chunk)
	}
}

// Attach adds a listener that receives every chunk from here on, and returns
// the function that removes it again. Chunks are delivered post-gain, exactly
// as they land in the recorder's own buffer, so a listener sees the same audio
// the recognizer would.
//
// Safe to call while capture is running -- which is the whole point, since the
// listener is attached when a dictation starts mid-meeting and dropped when it
// stops. The returned detach is idempotent.
func (r *Recorder) Attach(fn func([]float32)) func() {
	if fn == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.subs == nil {
		r.subs = map[int]func([]float32){}
	}
	id := r.nextSub
	r.nextSub++
	r.subs[id] = fn
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		delete(r.subs, id)
		r.mu.Unlock()
	}
}

// Level is the loudest moment since the previous call, on the meter's 0..1
// scale, gain included. Reading it clears the hold, so there is exactly one
// meter per recorder -- a second reader would eat the first one's peaks.
func (r *Recorder) Level() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	level := r.level
	r.level = 0
	return level
}

// Stop ends capture and returns the full recorded buffer.
func (r *Recorder) Stop() []float32 {
	if r.device != nil {
		r.device.Uninit()
		r.device = nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	buf := r.buf
	r.buf = nil
	r.level = 0
	return buf
}

// Close releases the underlying malgo context. The Recorder must not be
// used after Close.
func (r *Recorder) Close() {
	if r.device != nil {
		r.device.Uninit()
		r.device = nil
	}
	if r.ctx != nil {
		_ = r.ctx.Uninit()
		r.ctx.Free()
		r.ctx = nil
	}
}
