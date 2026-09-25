package history

import (
	"encoding/binary"
	"math"

	"voxlog-go/internal/voiceprint"
)

// A voice fingerprint is a few hundred float32s. SQLite has no array type,
// and JSON would triple the size and lose precision on the way through, so
// they are stored as the raw little-endian bytes -- fixed width, decoded with
// one loop, and directly comparable in Go.

// EncodeVec turns an embedding into its stored form. A nil or empty vector
// encodes as nil, which is the NULL a speaker with no fingerprint has.
func EncodeVec(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}

// DecodeVec reverses EncodeVec. A blob whose length is not a multiple of four
// cannot be an embedding this program wrote, so it decodes as nothing rather
// than as garbage a comparison would silently trust.
func DecodeVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// Cosine is the raw dot product of two stored fingerprints, unchanged since
// they were computed. Comparisons want voiceprint.Similarity instead, which
// is this over centred vectors; Cosine is left for the tests that check a
// vector survived a round trip through the database.
func Cosine(a, b []float32) float32 { return voiceprint.Cosine(a, b) }

// centroid averages fingerprints weighted by how much speech each represents,
// and renormalises: a voice built from a two-second "yes" and a twenty-minute
// presentation should sound like the presentation.
func centroid(vecs [][]float32, weights []float64) []float32 {
	return voiceprint.WeightedCentroid(vecs, weights)
}
