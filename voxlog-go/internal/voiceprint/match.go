package voiceprint

import "math"

// The matching half of the package: clustering fingerprints into people. The
// decisions that actually shape a transcript -- is this the same person as
// before? -- live here, where they can be tested without a 35 MB download.

// Thresholds, and which comparison each one is a threshold on.
//
// The two questions this package answers are not the same question, and they
// do not take the same arithmetic. Within one recording everyone shares a
// microphone, a codec and a room, so what two fingerprints have in common is
// mostly the recording; raw Cosine is what the numbers below were measured
// against, and centring there is unvalidated. Across recordings that shared
// part is exactly what misleads -- two takes of one speaker scored 0.66 to
// 0.84 and two different speakers 0.57 to 0.90, bands that overlap completely
// -- so those comparisons go through Similarity, which removes it first.
//
// MergeThreshold is a threshold on Cosine; IdentifyThreshold on Similarity.
const (
	// MergeThreshold joins two stretches of one meeting into one speaker.
	// Deliberately loose: merging two people reads as one confused speaker,
	// while splitting one person into three reads as a broken transcript, and
	// sherpa's own diarizer errs the same way (its clustering threshold is
	// 0.5).
	MergeThreshold = 0.55

	// IdentifyThreshold links a meeting's speaker to a voice that already has
	// a name, with no one asked -- across recordings, so on Similarity rather
	// than Cosine. Higher than MergeThreshold on purpose: a
	// wrong name spread across a user's whole history is a much worse failure
	// than an unnamed "Speaker 2", and the band between the two thresholds is
	// where the Voices pane asks instead of assuming.
	IdentifyThreshold = 0.72

	// MinSpeakerSeconds is how much speech a cluster needs before it is
	// allowed to stand as its own person. Below it, a cluster is almost
	// always a fragment of somebody already in the room.
	MinSpeakerSeconds = 3.0

	// FoldSlack is how much looser than MergeThreshold a fragment is judged
	// when it is being folded into a full speaker. A cluster of two or three
	// seconds has the least reliable centroid in the meeting, so judging it
	// at the same bar as a speaker with minutes behind them kept fragments
	// standing as people of their own.
	FoldSlack = 0.10
)

// Normalize returns v scaled to unit length. A zero vector comes back as nil:
// it carries no direction, so every similarity against it would be a
// meaningless zero rather than an honest "no answer".
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil
	}
	norm := float32(math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// Cosine is the similarity of two fingerprints. Vectors of different lengths
// (a model changed underfoot) or missing vectors score 0 rather than panic:
// "cannot tell" is a real answer here, and it is the safe one.
func Cosine(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return float32(dot)
}

// WeightedCentroid averages fingerprints by how much speech each one
// represents, and renormalises. Weighting matters: a voice profile built from
// a two-second "yes" and a twenty-minute presentation should sound like the
// presentation.
func WeightedCentroid(vecs [][]float32, weights []float64) []float32 {
	var sum []float32
	for i, v := range vecs {
		if len(v) == 0 {
			continue
		}
		w := 1.0
		if i < len(weights) && weights[i] > 0 {
			w = weights[i]
		}
		if sum == nil {
			sum = make([]float32, len(v))
		}
		if len(v) != len(sum) {
			continue
		}
		for j, x := range v {
			sum[j] += float32(float64(x) * w)
		}
	}
	return Normalize(sum)
}

// BlockSpeaker is one person as they appeared in one decoded block: the
// diarizer's own grouping, which is reliable within a block and meaningless
// between blocks.
type BlockSpeaker struct {
	// Block is the index of the block this grouping came from, and Local is
	// the diarizer's number for it inside that block. Together they are what
	// the caller maps back onto its turns.
	Block int
	Local int
	Embed []float32
	Secs  float64
}

// LinkBlocks decides which block speakers are the same person, and returns a
// meeting-wide id for each -- numbered by first appearance, so the reading
// order of a transcript is unchanged.
//
// It clusters block speakers rather than individual turns because the
// diarizer has already done the hard work inside each block, and one
// fingerprint over several seconds of a person is far steadier than one per
// reply.
//
// Speakers with no usable fingerprint -- under a second of audio in their
// block, which is what the embedder needs -- share one anonymous id between
// them rather than getting one each. Each of them used to become a person of
// their own, which is what turned an hour-long call with five people in it
// into a list of twenty-odd speakers: every stray "mhm" the diarizer heard in
// a block was somebody new. One "not heard well enough to tell" bucket is
// both closer to the truth and nameable as nothing, which is what these
// fragments are.
func LinkBlocks(speakers []BlockSpeaker, threshold float32) []int {
	ids := make([]int, len(speakers))
	for i := range ids {
		ids[i] = -1
	}

	type cluster struct {
		members  []int
		centroid []float32
	}
	var clusters []cluster

	// Where the un-fingerprinted speakers go, or -1 until the first one
	// appears. Reset with the clusters on every pass.
	unheard := -1

	assign := func(i int) {
		s := speakers[i]
		if len(s.Embed) == 0 {
			if unheard < 0 {
				clusters = append(clusters, cluster{})
				unheard = len(clusters) - 1
			}
			clusters[unheard].members = append(clusters[unheard].members, i)
			ids[i] = unheard
			return
		}
		best, bestScore := -1, threshold
		for c := range clusters {
			score := Cosine(s.Embed, clusters[c].centroid)
			if score >= bestScore {
				best, bestScore = c, score
			}
		}
		if best < 0 {
			clusters = append(clusters, cluster{members: []int{i}, centroid: s.Embed})
			ids[i] = len(clusters) - 1
			return
		}
		clusters[best].members = append(clusters[best].members, i)
		ids[i] = best
	}

	recentre := func() {
		for c := range clusters {
			vecs := make([][]float32, 0, len(clusters[c].members))
			weights := make([]float64, 0, len(clusters[c].members))
			for _, m := range clusters[c].members {
				vecs = append(vecs, speakers[m].Embed)
				weights = append(weights, speakers[m].Secs)
			}
			clusters[c].centroid = WeightedCentroid(vecs, weights)
		}
	}

	// Pass one is greedy and in block order, which means its early decisions
	// are made against centroids built from almost nothing. Passes two and
	// three rebuild every centroid from its members and reassign against
	// those -- deterministic, and enough to fix the early mistakes without
	// the cost or the instability of running to convergence.
	//
	// The centroids are held still for the length of each later pass: only
	// membership is rebuilt. Dropping the clusters and starting over, which
	// is what this did until the behaviour was measured, threw the recentred
	// centroids away and made passes two and three exact repeats of pass one.
	for i := range speakers {
		assign(i)
	}
	for pass := 0; pass < 2; pass++ {
		recentre()
		for c := range clusters {
			clusters[c].members = nil
		}
		for i := range ids {
			ids[i] = -1
		}
		for i := range speakers {
			assign(i)
		}
	}
	recentre()

	// A cluster with only a moment of speech in it is nearly always a
	// fragment of somebody already here. Fold it into its nearest neighbour
	// rather than inventing a person for it -- but only if there is a
	// neighbour it plausibly belongs to.
	total := make([]float64, len(clusters))
	for i, s := range speakers {
		total[ids[i]] += s.Secs
	}
	remap := make([]int, len(clusters))
	for c := range remap {
		remap[c] = c
	}
	for c := range clusters {
		if total[c] >= MinSpeakerSeconds || len(clusters) == 1 || c == unheard {
			continue
		}
		// Looser than the merge threshold, and it has to be: this cluster is
		// a few seconds long, so its centroid is the noisiest one in the
		// meeting, and holding a fragment to the same bar as a full speaker
		// is what left it standing as a person of its own.
		best, bestScore := -1, threshold-FoldSlack
		for other := range clusters {
			if other == c || total[other] < MinSpeakerSeconds {
				continue
			}
			if score := Cosine(clusters[c].centroid, clusters[other].centroid); score >= bestScore {
				best, bestScore = other, score
			}
		}
		if best >= 0 {
			remap[c] = best
		}
	}

	// Renumber by first appearance, so "Speaker 1" is whoever spoke first --
	// the same reading order the transcript had when speakers were numbered
	// one block at a time.
	order := map[int]int{}
	out := make([]int, len(speakers))
	for i := range speakers {
		c := remap[ids[i]]
		n, ok := order[c]
		if !ok {
			n = len(order)
			order[c] = n
		}
		out[i] = n
	}
	return out
}
