package embed

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"math"
)

// DeterministicEmbedder produces SHA-512-driven pseudo-vectors that are
// reproducible across runs and platforms. It has no semantic signal; its
// only purpose is to exercise code paths that need an embedder shape
// without depending on libonnxruntime or the BGE model (tests, local
// demos, Windows dev loops that cannot run BGEEmbedder).
//
// The output is L2-normalized so downstream cosine code sees the same
// numeric range as the real embedder.
type DeterministicEmbedder struct {
	dim int
}

// NewDeterministicEmbedder constructs a deterministic embedder reporting
// the package's canonical Dim.
func NewDeterministicEmbedder() *DeterministicEmbedder {
	return &DeterministicEmbedder{dim: Dim}
}

// Embed hashes text with SHA-512 to seed a 16-float tile, repeats the
// tile out to Dim, then L2-normalizes. Deterministic: same input always
// yields the same output regardless of platform, Go version, or runtime.
func (e *DeterministicEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dim := e.dim
	if dim <= 0 {
		dim = Dim
	}
	h := sha512.Sum512([]byte(text))
	// SHA-512 = 64 bytes = 16 float32 words. Decode the 16 words, tile
	// the slice to dim, then scrub NaN/Inf to keep norms well-defined.
	base := make([]float32, 16)
	for i := 0; i < 16; i++ {
		u := binary.LittleEndian.Uint32(h[i*4 : i*4+4])
		base[i] = math.Float32frombits(u)
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		out[i] = base[i%16]
	}
	var sum float64
	for i, v := range out {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			out[i] = 0
			continue
		}
		sum += float64(v) * float64(v)
	}
	if sum > 0 {
		inv := float32(1.0 / math.Sqrt(sum))
		for i := range out {
			out[i] *= inv
		}
	}
	return out, nil
}

// Dimension returns the output dimension (Dim unless overridden at
// construction, which the public constructor does not currently allow).
func (e *DeterministicEmbedder) Dimension() int {
	if e.dim <= 0 {
		return Dim
	}
	return e.dim
}
