package permissions

import (
	"time"

	"voxlog-go/internal/audio"
)

// RequestMicrophone triggers macOS's native mic-permission prompt if the
// state is still "not determined", by actually opening (briefly) and
// closing a capture device -- the same trigger a real dictation would hit,
// reused here so this package doesn't need its own Objective-C block-based
// AVCaptureDevice.requestAccessForMediaType:completionHandler: binding.
// A no-op once the user has already decided (authorized, denied, or
// restricted): opening a device again won't re-prompt or change anything.
func RequestMicrophone() error {
	if Microphone() != MicNotDetermined {
		return nil
	}
	rec, err := audio.NewRecorder("", 1.0)
	if err != nil {
		return err
	}
	defer rec.Close()
	if err := rec.Start(); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	rec.Stop()
	return nil
}
