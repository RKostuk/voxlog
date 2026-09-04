package history

import (
	"encoding/binary"
	"math"
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

// Cosine is how alike two fingerprints are, from 1 (the same voice) down to 0
// (unrelated). The vectors are stored already normalised, so this is a plain
// dot product.
//
// A copy of the same arithmetic that lives in internal/voiceid, kept here on
// purpose: that package loads an ONNX model through cgo, and the storage
// layer must not drag a 35 MB model dependency into every build and test that
// touches history.
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

// centroid averages fingerprints weighted by how much speech each represents,
// and renormalises: a voice built from a two-second "yes" and a twenty-minute
// presentation should sound like the presentation.
func centroid(vecs [][]float32, weights []float64) []float32 {
	var (
		sum  []float32
		norm float64
	)
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
	for _, x := range sum {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return nil
	}
	scale := float32(math.Sqrt(norm))
	for i := range sum {
		sum[i] /= scale
	}
	return sum
}
