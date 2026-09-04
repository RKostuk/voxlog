package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestLevelEmptyChunkIsZero(t *testing.T) {
	if got := Level(nil); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
	if got := Level([]float32{}); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestLevelSilenceIsZero(t *testing.T) {
	if got := Level(make([]float32, 512)); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestLevelIsScaledInDecibels(t *testing.T) {
	chunk := make([]float32, 100)
	for i := range chunk {
		chunk[i] = 0.05
	}
	want := (20*math.Log10(0.05) - meterFloorDB) / (meterCeilingDB - meterFloorDB)
	if got := Level(chunk); math.Abs(got-want) > 1e-6 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLevelReadsPeakNotAverage(t *testing.T) {
	// An average is always well below the peaks it is made of, so an RMS
	// meter scaled to full scale can never reach the top on real speech --
	// which read as "turning the gain up does nothing".
	chunk := make([]float32, 100) // mostly quiet
	for i := range chunk {
		chunk[i] = 0.001
	}
	chunk[50] = 0.5 // one loud moment

	if got := Level(chunk); got < 0.5 {
		t.Fatalf("a chunk peaking at 0.5 reads %v; the meter is following the average", got)
	}
}

func TestGainPinsTheMeterWhereItShould(t *testing.T) {
	// The calibration the user asked for: a normal voice at 100% sits high
	// with room left, 200% is nearly at the end, and 400% pins -- because at
	// that setting the recording really is too hot.
	speechPeak := float32(0.15) // ordinary voice on a laptop mic

	at := func(gain float64) float64 {
		chunk := make([]float32, 64)
		for i := range chunk {
			chunk[i] = speechPeak
		}
		r := &Recorder{gain: gain}
		r.onData(nil, pcmBytes(chunk), uint32(len(chunk)))
		return r.Level()
	}

	if got := at(1.0); got < 0.55 || got > 0.8 {
		t.Errorf("100%% reads %v, want high but not full", got)
	}
	if got := at(2.0); got < 0.8 || got >= 1.0 {
		t.Errorf("200%% reads %v, want near the top", got)
	}
	if got := at(4.0); got < 1.0 {
		t.Errorf("400%% reads %v, want pinned", got)
	}
}

func TestLevelIsLogarithmic(t *testing.T) {
	// The point of the dB scale: every doubling of amplitude moves the meter
	// by the same amount, so the whole bar is usable instead of speech living
	// in the bottom fifth and everything louder sitting pinned at the top.
	at := func(amp float32) float64 {
		chunk := make([]float32, 100)
		for i := range chunk {
			chunk[i] = amp
		}
		return Level(chunk)
	}
	stepA := at(0.04) - at(0.02)
	stepB := at(0.16) - at(0.08)
	if math.Abs(stepA-stepB) > 1e-6 {
		t.Fatalf("doubling moved the meter by %v low down and %v high up", stepA, stepB)
	}
}

func TestLevelPutsOrdinarySpeechMidScale(t *testing.T) {
	// A meter is only useful if the sound you actually make lands where you
	// can see it move in both directions.
	chunk := make([]float32, 200)
	for i := range chunk {
		chunk[i] = 0.06 // ordinary speaking voice
	}
	if got := Level(chunk); got < 0.45 || got > 0.85 {
		t.Fatalf("ordinary speech reads %v, want high on the scale", got)
	}
}

func TestLevelClampsToOne(t *testing.T) {
	chunk := make([]float32, 100)
	for i := range chunk {
		chunk[i] = 1.0 // full scale: 0 dBFS, the clipping point
	}
	if got := Level(chunk); got != 1 {
		t.Fatalf("got %v, want 1 -- the meter is a 0..1 amplitude", got)
	}
}

func TestLevelIgnoresSign(t *testing.T) {
	pos := []float32{0.3, 0.3, 0.3}
	neg := []float32{-0.3, -0.3, -0.3}
	if Level(pos) != Level(neg) {
		t.Fatalf("RMS must not depend on sign: %v vs %v", Level(pos), Level(neg))
	}
}

// pcmBytes encodes samples the way miniaudio hands them to the capture
// callback: little-endian float32, one channel.
func pcmBytes(samples []float32) []byte {
	out := make([]byte, len(samples)*4)
	for i, s := range samples {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(s))
	}
	return out
}

func TestOnDataBuffersSamplesUnchangedAtUnityGain(t *testing.T) {
	r := &Recorder{gain: 1.0}
	in := []float32{0.1, -0.2, 0.3}
	r.onData(nil, pcmBytes(in), uint32(len(in)))

	got := r.Stop()
	if len(got) != len(in) {
		t.Fatalf("got %d samples, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("sample %d: got %v, want %v", i, got[i], in[i])
		}
	}
}

func TestOnDataAppliesGainAndClips(t *testing.T) {
	r := &Recorder{gain: 4.0}
	in := []float32{0.1, -0.1, 0.5, -0.5}
	r.onData(nil, pcmBytes(in), uint32(len(in)))

	got := r.Stop()
	want := []float32{0.4, -0.4, 1.0, -1.0} // last two clip instead of wrapping
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Fatalf("sample %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestOnDataAccumulatesAcrossCallbacks(t *testing.T) {
	r := &Recorder{gain: 1.0}
	r.onData(nil, pcmBytes([]float32{0.1, 0.2}), 2)
	r.onData(nil, pcmBytes([]float32{0.3}), 1)

	if got := r.Stop(); len(got) != 3 || got[2] != 0.3 {
		t.Fatalf("got %v, want three accumulated samples", got)
	}
}

func TestOnDataInvokesStreamingCallbackWithTheChunk(t *testing.T) {
	var seen [][]float32
	r := &Recorder{gain: 2.0, onChunk: func(c []float32) { seen = append(seen, c) }}
	r.onData(nil, pcmBytes([]float32{0.25, 0.25}), 2)

	if len(seen) != 1 || len(seen[0]) != 2 {
		t.Fatalf("got %v, want one 2-sample chunk", seen)
	}
	// The callback sees the gained samples, the same ones that get buffered:
	// the live decoder must not hear something different from the batch path.
	if math.Abs(float64(seen[0][0]-0.5)) > 1e-6 {
		t.Fatalf("callback got %v, want gain applied (0.5)", seen[0][0])
	}
}

func TestStopDrainsBufferSoNextTakeStartsEmpty(t *testing.T) {
	r := &Recorder{gain: 1.0}
	r.onData(nil, pcmBytes([]float32{0.1}), 1)
	if got := r.Stop(); len(got) != 1 {
		t.Fatalf("got %v, want the recorded sample", got)
	}
	if got := r.Stop(); len(got) != 0 {
		t.Fatalf("got %v, want empty -- Stop must not hand back the previous take", got)
	}
}

func TestRecorderLevelFollowsGain(t *testing.T) {
	// The meter is how the user judges what the gain slider does, so moving
	// the slider has to move it. Measuring before gain made it slider-proof:
	// dragging changed nothing on screen.
	in := make([]float32, 200)
	for i := range in {
		in[i] = 0.05
	}

	quiet := &Recorder{gain: 1.0}
	quiet.onData(nil, pcmBytes(in), uint32(len(in)))

	loud := &Recorder{gain: 4.0}
	loud.onData(nil, pcmBytes(in), uint32(len(in)))

	// Read once each: reading clears the peak hold.
	quietLevel, loudLevel := quiet.Level(), loud.Level()
	if !(loudLevel > quietLevel) {
		t.Fatalf("gain 4 reads %v, gain 1 reads %v -- the slider must move the meter", loudLevel, quietLevel)
	}
	want := (20*math.Log10(0.05) - meterFloorDB) / (meterCeilingDB - meterFloorDB)
	if math.Abs(quietLevel-want) > 1e-6 {
		t.Fatalf("level is %v, want %v", quietLevel, want)
	}
}

func TestMeterHasHeadroomAtTheDefaultGain(t *testing.T) {
	// The whole complaint: at the old scaling an ordinary voice pinned the
	// meter as soon as the slider passed 1, so the bars stood flat and the
	// slider appeared to do nothing. Normal speech at the shipped default
	// must land mid-scale, leaving room to see louder AND quieter.
	speech := make([]float32, 200)
	for i := range speech {
		speech[i] = 0.06 // ordinary speaking voice
	}
	r := &Recorder{gain: 2.0} // settings.DefaultSettings().MicGain
	r.onData(nil, pcmBytes(speech), uint32(len(speech)))

	if got := r.Level(); got < 0.6 {
		t.Fatalf("normal speech reads %v at the default gain, want high on the scale", got)
	}
}

func TestRecorderLevelStillTracksTheVoice(t *testing.T) {
	soft := &Recorder{gain: 1.0}
	soft.onData(nil, pcmBytes(make([]float32, 100)), 100)

	loud := &Recorder{gain: 1.0}
	speech := make([]float32, 100)
	for i := range speech {
		speech[i] = 0.08
	}
	loud.onData(nil, pcmBytes(speech), 100)

	loudLevel, softLevel := loud.Level(), soft.Level()
	if !(loudLevel > softLevel) {
		t.Fatalf("speech reads %v, silence reads %v", loudLevel, softLevel)
	}
}

func TestRecorderLevelResetsOnStop(t *testing.T) {
	r := &Recorder{gain: 1.0}
	speech := make([]float32, 100)
	for i := range speech {
		speech[i] = 0.08
	}
	r.onData(nil, pcmBytes(speech), 100)
	r.Stop()
	if r.Level() != 0 {
		t.Fatalf("level is %v after Stop, want 0 -- the meter would keep the last take's reading", r.Level())
	}
}

func TestRecorderLevelHoldsThePeakBetweenReads(t *testing.T) {
	// Audio arrives every 10ms; the meters read at 25Hz. Keeping only the
	// newest chunk threw away three readings in four, so a short loud moment
	// -- most consonants -- could fall between two reads and never show.
	r := &Recorder{gain: 1.0}
	quiet := make([]float32, 160)
	for i := range quiet {
		quiet[i] = 0.01
	}
	loud := make([]float32, 160)
	for i := range loud {
		loud[i] = 0.3
	}

	r.onData(nil, pcmBytes(loud), 160)  // the transient
	r.onData(nil, pcmBytes(quiet), 160) // and three quiet chunks after it
	r.onData(nil, pcmBytes(quiet), 160)
	r.onData(nil, pcmBytes(quiet), 160)

	held := r.Level()
	if held < Level(loud)-1e-9 {
		t.Fatalf("meter read %v, want the transient's %v", held, Level(loud))
	}
	// And the hold clears, so the next read reports the next moment rather
	// than that same peak forever.
	r.onData(nil, pcmBytes(quiet), 160)
	if next := r.Level(); next >= held {
		t.Fatalf("after the peak, meter still reads %v (was %v)", next, held)
	}
}
