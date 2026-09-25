package diarize

import (
	"sort"

	"voxlog-go/internal/audio"
	"voxlog-go/internal/voiceprint"
)

// Diarizing a whole recording rather than a minute of one.
//
// The segmentation model works on a sliding window of its own and the
// clustering behind it only ever sees what it is handed, so handing it sixty
// seconds at a time means the question "is this the same person as before?" is
// never asked across the seam -- and the answer is re-invented, with fresh
// speaker numbers, every minute. Whole recordings are what the rest of the
// world diarizes; this file is how that is done here without holding an hour
// of audio and an hour of model state at once.

const (
	// WindowSeconds is how much of a recording is diarized in one pass.
	//
	// Ten minutes rather than the whole file: an hour of 16kHz mono is 230MB
	// resident before the model has allocated anything, and a decode that
	// scales with call length is how a long meeting becomes a hang. Ten
	// minutes is long enough that a speaker's whole turn-taking pattern is
	// inside one window, which is what the clustering actually needs.
	WindowSeconds = 600

	// OverlapSeconds is how much of the previous window each new one repeats.
	//
	// Nothing is diarized twice: the overlap is cut down the middle (see
	// acceptRange) and each half is credited to the window that saw more
	// context around it. The repeat exists so that a speaker change near a
	// seam is judged by a model that has heard both sides of it.
	OverlapSeconds = 30
)

const (
	// minFingerprintSeconds is the shortest segment allowed to vote on who a
	// window's speaker is. Shorter stretches stay in the transcript -- they
	// are words somebody said -- they just do not get to describe a voice,
	// because at a second and a half CAM++ returns a vector that matches
	// everybody.
	minFingerprintSeconds = 1.5

	// fingerprintWindowSeconds and maxFingerprintWindows cap what one
	// speaker's fingerprint is built from: a few seconds of continuous
	// speech, a few times over.
	//
	// Continuous is the point. Concatenating a speaker's scattered replies
	// into one buffer puts a splice every few hundred milliseconds, and a
	// splice is a transient no speaker model was trained on -- it is heard as
	// part of the voice. Separate windows, averaged afterwards, carry the same
	// amount of speech with none of the invented edges.
	fingerprintWindowSeconds = 4
	maxFingerprintWindows    = 3
)

// Fingerprinter is the half of voiceid.Embedder this file needs: turn speech
// into a vector. An interface rather than the concrete type so the stitching
// can be tested without a 35MB model on disk.
type Fingerprinter interface {
	Compute(samples []float32) []float32
}

// Windowed diarizes a whole recording and returns its segments in start-time
// order, timed from the start of the recording, with speaker numbers that mean
// the same thing from the first second to the last.
//
// A recording short enough to fit in one window still goes through the
// stitching, and that is not a formality. The clustering inside the model
// compares its own uncentred embeddings, where a voice is a small part of what
// a vector describes (see voiceid/center.go), so it is set to split rather
// than merge -- one person coming back as two is repaired here, in the space
// where the comparison means something, while two people merged into one is
// not repairable at all.
//
// f may be nil, and is when the embedding model has not been downloaded yet.
// Then windows cannot be stitched by voice and each one keeps its own
// numbering offset so that two windows never claim to share a speaker -- worse
// than the real answer, but honest, and it is the same "no fingerprints means
// no cross-window identity" contract the rest of the app has.
func Windowed(d *Diarizer, f Fingerprinter, samples []float32, maxGap, minDuration float32) []Segment {
	if d == nil || len(samples) == 0 {
		return nil
	}
	windows := windowBounds(len(samples), WindowSeconds, OverlapSeconds, audio.SampleRate)
	in := make([]state, 0, len(windows))
	for _, b := range windows {
		in = append(in, state{bounds: b, samples: samples[b.from:b.to]})
	}
	return stitch(d, f, in, windows, maxGap, minDuration)
}

// stitch diarizes each window and joins the results into one numbering. It is
// the body of both Windowed and WindowedFile; they differ only in where the
// samples come from.
func stitch(d *Diarizer, f Fingerprinter, in []state, windows []bounds, maxGap, minDuration float32) []Segment {
	if d == nil || len(in) == 0 {
		return nil
	}

	type local struct {
		window  int
		speaker int
	}
	var (
		all   []Segment
		owner []local // parallel to all
		heard []voiceprint.BlockSpeaker
		index = map[local]int{}
	)

	for w, window := range in {
		b := window.bounds
		raw := d.Process(window.samples)
		offset := float32(b.from) / float32(audio.SampleRate)

		lo, hi := acceptRange(b, windows, audio.SampleRate)
		kept := clip(Merge(raw, maxGap, minDuration), lo-offset, hi-offset)

		bySpeaker := map[int][]Segment{}
		var speakers []int
		for _, s := range kept {
			if _, seen := bySpeaker[s.Speaker]; !seen {
				speakers = append(speakers, s.Speaker)
			}
			bySpeaker[s.Speaker] = append(bySpeaker[s.Speaker], s)
		}
		// In first-appearance order rather than map order, so a recording
		// diarizes to the same thing twice running.
		for _, speaker := range speakers {
			segs := bySpeaker[speaker]
			index[local{w, speaker}] = len(heard)
			bs := voiceprint.BlockSpeaker{Block: w, Local: speaker}
			for _, s := range segs {
				bs.Secs += float64(s.End - s.Start)
			}
			if f != nil {
				bs.Embed = Fingerprint(f, window.samples, segs)
			}
			heard = append(heard, bs)
		}
		for _, s := range kept {
			s.Start += offset
			s.End += offset
			all = append(all, s)
			owner = append(owner, local{w, s.Speaker})
		}
	}

	ids := renumber(heard, d.link, f != nil && len(in) > 1)
	for i := range all {
		all[i].Speaker = ids[index[owner[i]]]
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Start < all[j].Start })

	// One more merge across the whole recording: a turn cut in half by a
	// window boundary is two segments of one person with no gap between them,
	// and after the stitching they finally carry the same number.
	return byFirstAppearance(Merge(all, maxGap, minDuration))
}

// byFirstAppearance renumbers speakers 0, 1, 2 in the order they first talk,
// so a three-person call is Speaker 1 to Speaker 3 rather than whichever
// numbers the model's own clustering happened to leave behind.
func byFirstAppearance(segments []Segment) []Segment {
	order := map[int]int{}
	for i, s := range segments {
		n, ok := order[s.Speaker]
		if !ok {
			n = len(order)
			order[s.Speaker] = n
		}
		segments[i].Speaker = n
	}
	return segments
}

// renumber turns per-window speakers into recording-wide ones. With
// fingerprints that is voiceid's clustering; without them, every window's
// speakers are kept apart, because two windows that cannot be compared must
// not be assumed to agree.
func renumber(heard []voiceprint.BlockSpeaker, threshold float32, linked bool) []int {
	if linked {
		return voiceprint.LinkBlocks(heard, threshold)
	}
	out := make([]int, len(heard))
	for i, h := range heard {
		out[i] = h.Block*maxSpeakersPerWindow + h.Local
	}
	return out
}

// maxSpeakersPerWindow is only an offset for the unlinked case above: window
// 2's "speaker 1" must not collide with window 1's.
const maxSpeakersPerWindow = 100

// bounds is one window of samples, as indices into the recording.
type bounds struct{ from, to int }

// windowBounds cuts a recording into overlapping windows. A recording shorter
// than one window comes back as one window covering all of it, which is what
// keeps every short meeting on exactly the path it was on before.
func windowBounds(total, windowSecs, overlapSecs, rate int) []bounds {
	size := windowSecs * rate
	if total <= size {
		return []bounds{{0, total}}
	}
	step := (windowSecs - overlapSecs) * rate
	var out []bounds
	for from := 0; from < total; from += step {
		to := from + size
		if to >= total {
			out = append(out, bounds{from, total})
			break
		}
		out = append(out, bounds{from, to})
	}
	return out
}

// acceptRange is the stretch of the recording a window is allowed to speak
// for, in seconds: its own span, minus half of each overlap it shares with a
// neighbour. Cutting the overlap down the middle is what keeps a speaker
// change near a seam from being reported twice, while still letting the model
// that heard the most context around it be the one that reports it.
func acceptRange(b bounds, all []bounds, rate int) (lo, hi float32) {
	lo = float32(b.from) / float32(rate)
	hi = float32(b.to) / float32(rate)
	for i, o := range all {
		if o == b {
			if i > 0 {
				lo = (float32(all[i-1].to) + float32(b.from)) / 2 / float32(rate)
			}
			if i < len(all)-1 {
				hi = (float32(b.to) + float32(all[i+1].from)) / 2 / float32(rate)
			}
			return lo, hi
		}
	}
	return lo, hi
}

// clip drops the segments outside [lo, hi) and trims the ones that straddle
// it. A trimmed segment is not lost: its other half belongs to the
// neighbouring window, which reports it, and the final Merge joins the two
// back together once both carry the same speaker number.
func clip(segments []Segment, lo, hi float32) []Segment {
	out := make([]Segment, 0, len(segments))
	for _, s := range segments {
		if s.End <= lo || s.Start >= hi {
			continue
		}
		if s.Start < lo {
			s.Start = lo
		}
		if s.End > hi {
			s.End = hi
		}
		if s.End > s.Start {
			out = append(out, s)
		}
	}
	return out
}

// Fingerprint describes one speaker: a few separate stretches of their
// continuous speech, averaged by how long each ran. Exported because
// tools/diareval measures this policy against the older one, which spliced a
// speaker's scattered replies into one buffer.
func Fingerprint(f Fingerprinter, samples []float32, segments []Segment) []float32 {
	var (
		vecs    [][]float32
		weights []float64
	)
	for _, s := range pickFingerprintSegments(segments) {
		slice := Slice(samples, s, audio.SampleRate)
		v := f.Compute(slice)
		if len(v) == 0 {
			continue
		}
		vecs = append(vecs, v)
		weights = append(weights, float64(s.End-s.Start))
	}
	return voiceprint.WeightedCentroid(vecs, weights)
}

// pickFingerprintSegments chooses the few stretches of continuous speech a
// speaker is described by: the longest ones, trimmed to a few seconds each.
// Longest first because the steadiest description of a voice is its longest
// uninterrupted stretch, and short ones are dropped entirely -- at a second
// and a half CAM++ returns a vector that matches everybody.
func pickFingerprintSegments(segments []Segment) []Segment {
	long := make([]Segment, 0, len(segments))
	for _, s := range segments {
		if s.End-s.Start >= minFingerprintSeconds {
			long = append(long, s)
		}
	}
	sort.SliceStable(long, func(i, j int) bool {
		return long[i].End-long[i].Start > long[j].End-long[j].Start
	})
	if len(long) > maxFingerprintWindows {
		long = long[:maxFingerprintWindows]
	}
	for i := range long {
		if long[i].End-long[i].Start > fingerprintWindowSeconds {
			long[i].End = long[i].Start + fingerprintWindowSeconds
		}
	}
	return long
}

// WindowedFile is Windowed over a recording on disk, which is how the app uses
// it: an hour of a call is 230MB of float32 and there is no reason for all of
// it to be resident at once. Only one window plus its overlap ever is.
func WindowedFile(d *Diarizer, f Fingerprinter, path string, maxGap, minDuration float32) ([]Segment, error) {
	r, err := audio.OpenWAV(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	total := int(r.Samples())
	windows := windowBounds(total, WindowSeconds, OverlapSeconds, audio.SampleRate)

	// buf holds the recording from `at` onwards: each window is served from
	// it, and everything before the next window's start is dropped once that
	// window has been handed over.
	var (
		buf []float32
		at  int
		in  = make([]state, 0, len(windows))
	)
	for _, b := range windows {
		for at+len(buf) < b.to {
			chunk, err := r.Read(b.to - at - len(buf))
			if len(chunk) == 0 || err != nil {
				break
			}
			buf = append(buf, chunk...)
		}
		from := b.from - at
		to := min(b.to-at, len(buf))
		if from < 0 || from >= to {
			break
		}
		in = append(in, state{bounds: b, samples: buf[from:to]})

		// Keep only what a later window still needs.
		if next := b.to - OverlapSeconds*audio.SampleRate; next > at {
			buf = append([]float32(nil), buf[min(next-at, len(buf)):]...)
			at = next
		}
	}
	return stitch(d, f, in, windows, maxGap, minDuration), nil
}

// state is one window and the audio it covers, held only for as long as that
// window is being diarized.
type state struct {
	bounds  bounds
	samples []float32
}

// FingerprintFile is Fingerprint against a recording on disk: it reads back
// only the few seconds of speech a fingerprint is built from, rather than the
// whole call, which is what lets a finished meeting be described without
// holding it in memory a second time.
func FingerprintFile(f Fingerprinter, path string, segments []Segment) []float32 {
	var (
		vecs    [][]float32
		weights []float64
	)
	for _, s := range pickFingerprintSegments(segments) {
		samples, err := audio.ReadRange(path, float64(s.Start), float64(s.End))
		if err != nil {
			continue
		}
		v := f.Compute(samples)
		if len(v) == 0 {
			continue
		}
		vecs = append(vecs, v)
		weights = append(weights, float64(s.End-s.Start))
	}
	return voiceprint.WeightedCentroid(vecs, weights)
}
