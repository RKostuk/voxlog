// Package systemaudio captures the machine's audio output (what's playing
// through the speakers) alongside the microphone, so dictation can include
// the other side of a call or a video being watched.
//
// Implemented on ScreenCaptureKit -- see tap_darwin.m. macOS classifies
// even audio-only capture as screen recording, so this needs Screen
// Recording permission, which is separate from the Microphone permission
// the mic path uses.
package systemaudio

/*
// -fobjc-arc is load-bearing, not tidiness: cgo compiles .m files WITHOUT
// ARC by default, and tap_darwin.m is written in ARC style. Under manual
// retain/release its __block-captured objects (the SCShareableContent the
// completion handler hands back, above all) are never retained, so they are
// already dead by the time the code uses them -- a SIGSEGV on the very
// first capture.
#cgo CFLAGS: -x objective-c -fobjc-arc -fmodules
#cgo LDFLAGS: -framework Foundation -framework ScreenCaptureKit -framework AVFoundation -framework CoreMedia

int voxlogSystemAudioStart(void);
void voxlogSystemAudioStop(void);
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// sink receives captured chunks while a capture is running. Guarded by mu:
// the Objective-C side delivers on its own dispatch queue, while Start/Stop
// are called from the dictation goroutine.
//
// running is what makes the single-capture rule explicit. There is one tap in
// the process (one sink, one ScreenCaptureKit stream), so a second Start would
// quietly steal the first one's audio -- which is exactly what a dictation
// started in the middle of a meeting would do.
var (
	mu      sync.Mutex
	sink    func([]float32)
	running bool
)

// ErrBusy is returned by Start when a capture is already running. Callers are
// expected to carry on without system audio rather than treat this as failure:
// during a meeting it is the normal outcome, not an error.
var ErrBusy = errors.New("system audio capture is already in use")

// Running reports whether a capture is in progress.
func Running() bool {
	mu.Lock()
	defer mu.Unlock()
	return running
}

//export goSystemAudioChunk
func goSystemAudioChunk(samples *C.float, count C.int) {
	mu.Lock()
	f := sink
	mu.Unlock()
	if f == nil || count <= 0 {
		return
	}

	// Copy out of the C buffer: it's freed as soon as this returns, and the
	// consumer may hold onto the slice.
	src := unsafe.Slice((*float32)(unsafe.Pointer(samples)), int(count))
	chunk := make([]float32, len(src))
	copy(chunk, src)
	f(chunk)
}

// startErrors maps the C layer's status codes to something a user-facing
// message can be built from.
var startErrors = map[int]error{
	1: errors.New("timed out querying shareable content"),
	2: errors.New("no shareable content (grant Screen Recording permission in System Settings)"),
	3: errors.New("could not attach the audio output"),
	4: errors.New("timed out starting capture"),
	5: errors.New("capture failed to start"),
	6: errors.New("system audio capture needs macOS 13 or newer"),
}

// Start begins capturing system audio, delivering 16 kHz mono chunks to
// onChunk until Stop is called. Safe to call when already running (no-op).
func Start(onChunk func([]float32)) error {
	mu.Lock()
	if running {
		mu.Unlock()
		return ErrBusy
	}
	sink = onChunk
	running = true
	mu.Unlock()

	if rc := int(C.voxlogSystemAudioStart()); rc != 0 {
		mu.Lock()
		sink = nil
		running = false
		mu.Unlock()
		if err, ok := startErrors[rc]; ok {
			return err
		}
		return fmt.Errorf("system audio capture failed (code %d)", rc)
	}
	return nil
}

// Stop ends capture. Safe to call when nothing is running.
func Stop() {
	mu.Lock()
	sink = nil
	running = false
	mu.Unlock()
	C.voxlogSystemAudioStop()
}
