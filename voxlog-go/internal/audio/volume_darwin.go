package audio

/*
#cgo LDFLAGS: -framework CoreAudio
#include <CoreAudio/CoreAudio.h>

// The input device's own volume -- the same control System Settings shows
// under Sound > Input, and the thing that actually decides how loud the
// microphone records.
//
// This matters because multiplying samples afterwards is not the same lever
// at all: it scales a signal that has already been captured, noise and
// quantization included, and cannot recover detail the converter never
// resolved. A machine sitting at input volume 8/100 records almost nothing,
// and no amount of arithmetic downstream fixes that.
//
// Not every device exposes a settable volume (many USB interfaces have a
// physical knob instead and report the property as read-only), hence the
// separate "is it settable" answer rather than a silent no-op.

static AudioObjectID defaultInputDevice(void) {
	AudioObjectID device = kAudioObjectUnknown;
	UInt32 size = sizeof(device);
	AudioObjectPropertyAddress addr = {
		kAudioHardwarePropertyDefaultInputDevice,
		kAudioObjectPropertyScopeGlobal,
		kAudioObjectPropertyElementMain,
	};
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, &device) != noErr) {
		return kAudioObjectUnknown;
	}
	return device;
}

// volumeAddress fills addr for the given channel: 0 is the device-wide
// "master" control, 1 and 2 are the individual channels. Devices differ over
// which of these they implement, so callers try master first and fall back.
static AudioObjectPropertyAddress volumeAddress(UInt32 channel) {
	AudioObjectPropertyAddress addr = {
		kAudioDevicePropertyVolumeScalar,
		kAudioDevicePropertyScopeInput,
		channel,
	};
	return addr;
}

// getInputVolume writes the volume into *out and returns 1 on success.
static int getInputVolume(float *out) {
	AudioObjectID device = defaultInputDevice();
	if (device == kAudioObjectUnknown) {
		return 0;
	}
	for (UInt32 channel = 0; channel <= 2; channel++) {
		AudioObjectPropertyAddress addr = volumeAddress(channel);
		if (!AudioObjectHasProperty(device, &addr)) {
			continue;
		}
		Float32 value = 0;
		UInt32 size = sizeof(value);
		if (AudioObjectGetPropertyData(device, &addr, 0, NULL, &size, &value) == noErr) {
			*out = (float)value;
			return 1;
		}
	}
	return 0;
}

// setInputVolume returns 1 if the device accepted the new volume.
static int setInputVolume(float value) {
	AudioObjectID device = defaultInputDevice();
	if (device == kAudioObjectUnknown) {
		return 0;
	}
	int ok = 0;
	for (UInt32 channel = 0; channel <= 2; channel++) {
		AudioObjectPropertyAddress addr = volumeAddress(channel);
		if (!AudioObjectHasProperty(device, &addr)) {
			continue;
		}
		Boolean settable = false;
		if (AudioObjectIsPropertySettable(device, &addr, &settable) != noErr || !settable) {
			continue;
		}
		Float32 v = (Float32)value;
		if (AudioObjectSetPropertyData(device, &addr, 0, NULL, sizeof(v), &v) == noErr) {
			ok = 1;
			// Keep going: on a device that exposes per-channel controls,
			// setting only the first would leave the two channels mismatched.
		}
	}
	return ok;
}

static int inputVolumeSettable(void) {
	AudioObjectID device = defaultInputDevice();
	if (device == kAudioObjectUnknown) {
		return 0;
	}
	for (UInt32 channel = 0; channel <= 2; channel++) {
		AudioObjectPropertyAddress addr = volumeAddress(channel);
		if (!AudioObjectHasProperty(device, &addr)) {
			continue;
		}
		Boolean settable = false;
		if (AudioObjectIsPropertySettable(device, &addr, &settable) == noErr && settable) {
			return 1;
		}
	}
	return 0;
}
*/
import "C"

// InputVolume reports the default input device's volume, 0 to 1, and whether
// the device answered at all.
func InputVolume() (float64, bool) {
	var v C.float
	if C.getInputVolume(&v) == 0 {
		return 0, false
	}
	return float64(v), true
}

// SetInputVolume sets the default input device's volume, 0 to 1. It reports
// false when the device has no settable volume of its own, which is normal
// for interfaces with a physical gain knob.
func SetInputVolume(v float64) bool {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return C.setInputVolume(C.float(v)) != 0
}

// InputVolumeSettable reports whether this machine's input device lets its
// volume be changed from software.
func InputVolumeSettable() bool {
	return C.inputVolumeSettable() != 0
}
