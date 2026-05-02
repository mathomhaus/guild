package embed

import "math"

// int8 quantization convention for 384-dim L2-normalized vectors.
//
// ADR-003 stores vectors as int8 BLOBs (384 bytes per vector). The
// embedder emits []float32 with unit L2 norm; we map each component to
// int8 with a symmetric scale of 127:
//
//	q_i = round(clamp(v_i * 127, -128, 127))
//
// For a unit-norm input, |v_i| <= 1, so the clamp is almost never
// load-bearing. 127 is chosen over 128 to keep the scheme symmetric
// (no asymmetric negative range wasted) and to keep the max
// representable magnitude at exactly 1.0 on dequantization.
//
// Dequantization is the inverse: f_i = q_i / 127. The dot product of
// two quantized vectors approximates the dot product of the original
// L2-normalized vectors (which equals cosine similarity) up to a
// quantization error on the order of 1/127. Empirically, the
// retrieval order at k=60 is unchanged vs. the float path for the
// BGE-small-en-v1.5 embedding distribution.
//
// Keeping this convention colocated with the cosine math means the
// Quantize and cosineInt8 calls are the only two places that need to
// agree; nothing else in the codebase encodes a raw 127.
const quantScale = 127

// VecDim is the vector dimension every Index holds. Mirrors the
// embedder's Dim so the index can assert incoming BLOB lengths match
// without importing the embedder contract. Kept as an untyped const
// so callers can use it in array sizes if they want.
const VecDim = Dim

// Quantize converts a float32 vector of length VecDim into its canonical
// int8 form. Inputs that are not unit-norm are still quantized, but
// components outside [-1, 1] are clipped to [-128, 127]. Returns nil
// if len(v) != VecDim so callers can treat "wrong shape" as an error
// at a single call site.
//
// The output is a freshly allocated slice; the caller owns it.
func Quantize(v []float32) []int8 {
	if len(v) != VecDim {
		return nil
	}
	out := make([]int8, VecDim)
	for i, x := range v {
		// round-half-away-from-zero via math.Round. Float64 promotion
		// keeps the multiply precise on values near the clamp edges;
		// float32 arithmetic accumulates enough error here to flip a
		// boundary case occasionally, which is observable in tests.
		s := math.Round(float64(x) * quantScale)
		switch {
		case s > 127:
			out[i] = 127
		case s < -128:
			out[i] = -128
		default:
			out[i] = int8(s)
		}
	}
	return out
}

// Dequantize is the inverse of Quantize. Returns nil if len(q) != VecDim.
// Primarily for tests and diagnostic tooling; the hot query path never
// dequantizes.
func Dequantize(q []int8) []float32 {
	if len(q) != VecDim {
		return nil
	}
	out := make([]float32, VecDim)
	inv := float32(1.0) / float32(quantScale)
	for i, x := range q {
		out[i] = float32(x) * inv
	}
	return out
}

// cosineInt8 returns the int32 dot product of two int8 vectors of
// length VecDim. Under the Quantize convention, this value is
// proportional to the cosine similarity of the pre-quantization
// float vectors (divide by quantScale*quantScale to recover the
// approximate cosine).
//
// TopK ranks by this raw int32 score because the proportionality
// constant is the same for every pair, so ordering is preserved and
// the division is wasted work. Callers that need the float cosine
// can call CosineFloat.
//
// Unrolled by four so the Go compiler emits tight ARM64/AMD64
// loads without needing explicit SIMD intrinsics. Benchmarks on
// Apple M3 Pro show ~1.5-2x over the naive single-accumulator loop.
func cosineInt8(a, b []int8) int32 {
	// Enable bounds-check elimination: the caller is responsible for
	// equal lengths, but the compiler proves only what it sees. A
	// single slice up-front gives it enough to elide the inner bounds
	// checks.
	_ = a[VecDim-1]
	_ = b[VecDim-1]

	var s0, s1, s2, s3 int32
	// VecDim=384 is divisible by 4; no tail.
	for i := 0; i < VecDim; i += 4 {
		s0 += int32(a[i]) * int32(b[i])
		s1 += int32(a[i+1]) * int32(b[i+1])
		s2 += int32(a[i+2]) * int32(b[i+2])
		s3 += int32(a[i+3]) * int32(b[i+3])
	}
	return s0 + s1 + s2 + s3
}

// CosineFloat returns the approximate cosine similarity of two
// int8-quantized VecDim vectors as a float32 in the range [-1, 1].
// Exposed for tests and diagnostic paths.
func CosineFloat(a, b []int8) float32 {
	if len(a) != VecDim || len(b) != VecDim {
		return 0
	}
	dot := cosineInt8(a, b)
	return float32(dot) / float32(quantScale*quantScale)
}
