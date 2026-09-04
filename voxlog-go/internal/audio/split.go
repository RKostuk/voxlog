package audio

// Splitting a long recording into decodable blocks.
//
// A meeting is an hour of audio, and handing an hour to a recognizer in one
// call is two problems: the memory it takes to hold the features for all of
// it, and the minutes it spends inside a single uninterruptible call, during
// which a ten-second dictation recorded afterwards cannot be decoded at all.
// Blocks fix both -- the decoder returns regularly, which is where a waiting
// take gets its turn.
//
// A dictation never reaches this code: takes shorter than BlockSamples are
// decoded whole, exactly as before.

const (
	// BlockSamples is the nominal block length: 60 seconds. Long enough that
	// the per-call model overhead is noise, short enough that a queued
	// dictation waits seconds rather than minutes.
	BlockSamples = 60 * SampleRate
	// blockSearch is how far either side of the nominal boundary a quieter
	// place to cut is looked for.
	blockSearch = 2 * SampleRate
	// quietWindow is the width of the stretch being judged, 200ms -- about the
	// length of the pause between two words.
	quietWindow = SampleRate / 5
)

// CutPoint returns where a block starting at samples[0] should end if it
// nominally ends at target: the middle of the quietest 200ms within
// blockSearch of that point, so a cut lands in a pause rather than through a
// word. Falls back to target itself when there is no room to look around.
//
// ponytail: energy, not a VAD. It finds a pause in speech reliably enough,
// and cannot tell a pause from steady background noise -- if block seams start
// eating words, a real VAD is the upgrade, not a wider search.
func CutPoint(samples []float32, target int) int {
	if target >= len(samples) {
		return len(samples)
	}
	lo := max(0, target-blockSearch)
	hi := min(len(samples)-quietWindow, target+blockSearch)
	if hi <= lo {
		return target
	}

	// One running sum over the search range rather than a fresh sum per
	// candidate: the range is 4 seconds, i.e. 64000 candidate positions.
	var energy float64
	for i := lo; i < lo+quietWindow; i++ {
		energy += float64(samples[i]) * float64(samples[i])
	}
	best, bestEnergy := lo, energy
	for start := lo + 1; start <= hi; start++ {
		out := float64(samples[start-1]) * float64(samples[start-1])
		in := float64(samples[start+quietWindow-1]) * float64(samples[start+quietWindow-1])
		energy += in - out
		if energy < bestEnergy {
			best, bestEnergy = start, energy
		}
	}
	return best + quietWindow/2
}

// SplitBlocks cuts samples into blocks of at most about BlockSamples, each
// ending at a quiet point. A recording shorter than that comes back as one
// block, which is the case every dictation takes.
func SplitBlocks(samples []float32) [][]float32 {
	if len(samples) <= BlockSamples {
		return [][]float32{samples}
	}
	var blocks [][]float32
	for len(samples) > 0 {
		if len(samples) <= BlockSamples {
			blocks = append(blocks, samples)
			break
		}
		cut := CutPoint(samples, BlockSamples)
		blocks = append(blocks, samples[:cut])
		samples = samples[cut:]
	}
	return blocks
}

// ReadBlocks streams a WAV file through fn one block at a time, so an hour of
// audio is decoded without ever being resident. Stops at the first error fn
// returns.
func ReadBlocks(path string, fn func([]float32) error) error {
	r, err := OpenWAV(path)
	if err != nil {
		return err
	}
	defer r.Close()

	var carry []float32
	for {
		// Read a block plus the search margin, so the cut point has somewhere
		// to move to without needing the next read.
		chunk, err := r.Read(BlockSamples + blockSearch - len(carry))
		if err != nil {
			return err
		}
		if chunk == nil && len(carry) == 0 {
			return nil
		}
		buf := append(carry, chunk...)
		if chunk == nil || len(buf) <= BlockSamples {
			// The tail of the file: nothing left to cut against.
			if err := fn(buf); err != nil {
				return err
			}
			return nil
		}
		cut := CutPoint(buf, BlockSamples)
		if err := fn(buf[:cut]); err != nil {
			return err
		}
		// Copied, not resliced: buf's backing array is handed to fn, and the
		// next append would write into audio the decoder may still be holding.
		carry = append([]float32(nil), buf[cut:]...)
	}
}
