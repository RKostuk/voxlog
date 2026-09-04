package audio

import (
	"fmt"
	"math"

	"github.com/gen2brain/malgo"
)

// InputDevices lists the names of available capture devices, for the
// Settings window's input picker. The empty string (system default) is not
// included -- the UI supplies that option itself.
func InputDevices() ([]string, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()

	infos, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, fmt.Errorf("enumerate capture devices: %w", err)
	}

	names := make([]string, 0, len(infos))
	for i := range infos {
		names = append(names, infos[i].Name())
	}
	return names, nil
}

// The meter's window, in dBFS. It reads PEAK, not average: an average is
// always far below the peaks it is made of, so an RMS meter scaled to the
// clipping point can never reach the top on real speech no matter what the
// gain is set to -- which is exactly how "it does not get louder" looked.
//
// The top is -6 dBFS rather than 0: that is where a recording is as hot as
// it should ever get, with a little headroom left before samples actually
// clip. So a normal voice at 100% sits high but not full, 200% is close to
// the end, and 400% pins the meter -- which is the point, because at that
// setting it IS too much.
const (
	meterFloorDB   = -40.0
	meterCeilingDB = -6.0
)

// Level is the peak of one captured chunk on the meter's 0..1 scale.
func Level(chunk []float32) float64 {
	var peak float64
	for _, s := range chunk {
		if a := math.Abs(float64(s)); a > peak {
			peak = a
		}
	}
	return scaleLevel(peak)
}

// PeakDBFS reports the loudest sample in dBFS (0 is full scale, negative
// below), or -inf for silence. Used for logging what a real take actually
// looked like, which is the only way to tell "the gain is too low" apart
// from "the microphone is quiet" after the fact.
func PeakDBFS(samples []float32) float64 {
	var peak float64
	for _, s := range samples {
		if a := math.Abs(float64(s)); a > peak {
			peak = a
		}
	}
	if peak <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(peak)
}

// scaleLevel maps a peak amplitude onto the meter's 0..1 range in decibels.
//
// Decibels, not a plain multiplier, because loudness is logarithmic and a
// linear meter cannot show it: with linear scaling ordinary speech lives in
// the bottom fifth of the bar and everything above it sits pinned at the
// top, so the display reads as flat no matter how the constant is tuned.
func scaleLevel(peak float64) float64 {
	if peak <= 0 {
		return 0
	}
	db := 20 * math.Log10(peak) // dBFS: 0 is full scale, negative below
	return math.Max(0, math.Min(1, (db-meterFloorDB)/(meterCeilingDB-meterFloorDB)))
}
