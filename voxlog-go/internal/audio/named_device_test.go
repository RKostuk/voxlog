package audio

import "testing"

// Starting a capture on a device selected BY NAME used to panic before it
// recorded a single sample: the device ID resolved out of ctx.Devices lives in
// Go memory, and handing it to InitDevice unpinned trips cgocheck. The default
// device (a nil ID) never went down that path, so the crash only appeared once
// someone picked a microphone in Settings -- both dictation and meetings, on
// the first keypress.
//
// Needs real hardware, so it skips where there is none (CI).
func TestStartOnNamedDeviceDoesNotPanic(t *testing.T) {
	r, err := NewRecorder("", 1.0)
	if err != nil {
		t.Skipf("no audio context here: %v", err)
	}
	defer r.Close()

	infos, err := r.ctx.Devices(1) // malgo.Capture
	if err != nil || len(infos) == 0 {
		t.Skip("no capture devices on this machine")
	}
	name := infos[0].Name()

	named, err := NewRecorder(name, 1.0)
	if err != nil {
		t.Skipf("no audio context here: %v", err)
	}
	defer named.Close()

	if err := named.Start(); err != nil {
		t.Skipf("cannot open %q here: %v", name, err)
	}
	named.Stop()
}
