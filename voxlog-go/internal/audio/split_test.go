package audio

import (
	"math"
	"testing"
)

// toneSilenceTone builds speech-shaped audio with one silent gap, at the
// sample offset given.
func toneSilenceTone(total, gapStart, gapLen int) []float32 {
	out := make([]float32, total)
	for i := range out {
		out[i] = float32(math.Sin(float64(i) * 0.05))
	}
	for i := gapStart; i < gapStart+gapLen && i < total; i++ {
		out[i] = 0
	}
	return out
}

func TestCutPointLandsInTheSilence(t *testing.T) {
	// The gap sits half a second before the nominal boundary; the cut has to
	// move back to it rather than slicing through the tone.
	gapStart := BlockSamples - SampleRate/2
	gapLen := SampleRate / 4
	samples := toneSilenceTone(BlockSamples*2, gapStart, gapLen)

	cut := CutPoint(samples, BlockSamples)
	if cut < gapStart || cut > gapStart+gapLen {
		t.Fatalf("cut at %d, want inside the silence [%d, %d]", cut, gapStart, gapStart+gapLen)
	}
}

func TestCutPointStaysWithinTheSearchRange(t *testing.T) {
	// A quiet stretch far outside the search window must not drag the cut
	// there: blocks would drift further from their nominal length every time.
	samples := toneSilenceTone(BlockSamples*2, 100, SampleRate)
	cut := CutPoint(samples, BlockSamples)
	if cut < BlockSamples-blockSearch || cut > BlockSamples+blockSearch {
		t.Fatalf("cut at %d, want within %d of %d", cut, blockSearch, BlockSamples)
	}
}

func TestCutPointOnShortAudioIsTheEnd(t *testing.T) {
	samples := make([]float32, 1000)
	if got := CutPoint(samples, BlockSamples); got != len(samples) {
		t.Fatalf("got %d, want the whole thing (%d)", got, len(samples))
	}
}

func TestSplitBlocksLeavesShortTakesWhole(t *testing.T) {
	// Every dictation goes through here. One block, no copying, no seams.
	samples := make([]float32, 5*SampleRate)
	blocks := SplitBlocks(samples)
	if len(blocks) != 1 || len(blocks[0]) != len(samples) {
		t.Fatalf("got %d blocks, want the take unsplit", len(blocks))
	}
}

func TestSplitBlocksCoversEverythingExactlyOnce(t *testing.T) {
	// Losing or repeating samples at a seam would drop or duplicate words.
	total := BlockSamples*3 + 12345
	samples := toneSilenceTone(total, 0, 0)
	blocks := SplitBlocks(samples)
	if len(blocks) < 3 {
		t.Fatalf("got %d blocks for %d samples, want at least 3", len(blocks), total)
	}

	n := 0
	for _, b := range blocks {
		if len(b) == 0 {
			t.Fatal("an empty block would decode to nothing and waste a model call")
		}
		for _, s := range b {
			if s != samples[n] {
				t.Fatalf("sample %d differs; blocks are not a straight partition", n)
			}
			n++
		}
	}
	if n != total {
		t.Fatalf("blocks cover %d samples, want %d", n, total)
	}
}

func TestReadBlocksStreamsTheWholeFile(t *testing.T) {
	total := BlockSamples*2 + SampleRate
	path := writeWAV(t, toneSilenceTone(total, 0, 0))

	var blocks, samples int
	err := ReadBlocks(path, func(b []float32) error {
		blocks++
		samples += len(b)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadBlocks: %v", err)
	}
	if blocks < 3 {
		t.Fatalf("got %d blocks, want at least 3", blocks)
	}
	if samples != total {
		t.Fatalf("read %d samples, want %d -- audio was lost at a seam", samples, total)
	}
}

func TestReadBlocksShortFileIsOneBlock(t *testing.T) {
	path := writeWAV(t, make([]float32, SampleRate))
	blocks := 0
	if err := ReadBlocks(path, func([]float32) error { blocks++; return nil }); err != nil {
		t.Fatal(err)
	}
	if blocks != 1 {
		t.Fatalf("got %d blocks, want 1", blocks)
	}
}

func TestReadBlocksStopsOnError(t *testing.T) {
	path := writeWAV(t, make([]float32, BlockSamples*3))
	calls := 0
	err := ReadBlocks(path, func([]float32) error {
		calls++
		return errStop
	})
	if err != errStop {
		t.Fatalf("got %v, want the callback's error", err)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times after failing, want 1", calls)
	}
}

var errStop = errStopType{}

type errStopType struct{}

func (errStopType) Error() string { return "stop" }
