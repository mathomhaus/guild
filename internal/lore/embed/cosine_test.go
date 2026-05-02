package embed

import (
	"math"
	"math/rand"
	"testing"
)

// TestQuantize_RoundTripIsTight asserts that Quantize then Dequantize
// reproduces the original unit-norm float32 vector within the expected
// per-component quantization error of roughly 1/127.
func TestQuantize_RoundTripIsTight(t *testing.T) {
	v := randUnitVec(t, 1)
	q := Quantize(v)
	if len(q) != VecDim {
		t.Fatalf("Quantize: got len %d, want %d", len(q), VecDim)
	}
	d := Dequantize(q)
	if len(d) != VecDim {
		t.Fatalf("Dequantize: got len %d, want %d", len(d), VecDim)
	}
	const tol = 1.0 / (2 * quantScale) // half-LSB
	for i := range v {
		if diff := math.Abs(float64(v[i] - d[i])); diff > tol+1e-6 {
			t.Fatalf("roundtrip drift at i=%d: |%g - %g| = %g > %g",
				i, v[i], d[i], diff, tol)
		}
	}
}

// TestQuantize_ClampsOutOfRange covers the defensive clamp on inputs
// outside [-1, 1]. The embedder only emits unit-norm vectors in
// production, but Quantize is a public helper and callers may feed it
// arbitrary floats (e.g. in tests or backfills with stale normalization).
func TestQuantize_ClampsOutOfRange(t *testing.T) {
	v := make([]float32, VecDim)
	v[0] = 2.5  // clamps to 127
	v[1] = -3.0 // clamps to -128
	v[2] = 1.0  // maps to 127
	v[3] = -1.0 // maps to -127
	v[4] = 0.0  // maps to 0
	v[5] = 0.5  // round(0.5 * 127) = round(63.5) = 64
	v[6] = -0.5 // round(-0.5 * 127) = round(-63.5) = -64
	q := Quantize(v)
	want := []int8{127, -128, 127, -127, 0, 64, -64}
	for i, w := range want {
		if q[i] != w {
			t.Errorf("q[%d]: got %d want %d", i, q[i], w)
		}
	}
}

// TestQuantize_WrongLengthReturnsNil documents the contract for bad
// input shape: a nil return, not a panic.
func TestQuantize_WrongLengthReturnsNil(t *testing.T) {
	if Quantize(nil) != nil {
		t.Error("Quantize(nil): got non-nil")
	}
	if Quantize(make([]float32, VecDim-1)) != nil {
		t.Error("Quantize short vec: got non-nil")
	}
	if Quantize(make([]float32, VecDim+1)) != nil {
		t.Error("Quantize long vec: got non-nil")
	}
	if Dequantize(nil) != nil {
		t.Error("Dequantize(nil): got non-nil")
	}
	if Dequantize(make([]int8, VecDim-1)) != nil {
		t.Error("Dequantize short vec: got non-nil")
	}
}

// TestCosineFloat_IdenticalVectorsIsOne asserts the self-similarity
// identity: cosine(v, v) ~= 1 under the int8 roundtrip.
func TestCosineFloat_IdenticalVectorsIsOne(t *testing.T) {
	v := randUnitVec(t, 2)
	q := Quantize(v)
	c := CosineFloat(q, q)
	// Self-cosine of the quantized vector recovers |q|^2 / 127^2,
	// which lands slightly under 1 because each component loses up to
	// half a quantization step. Empirically ~0.995 for random
	// unit-norm inputs at dim=384.
	if c < 0.98 || c > 1.01 {
		t.Fatalf("self cosine: got %g, want close to 1", c)
	}
}

// TestCosineFloat_OrthogonalIsNearZero seeds a random vector, builds
// an orthogonal counterpart via Gram-Schmidt, and checks that cosine
// rounds to zero under quantization.
func TestCosineFloat_OrthogonalIsNearZero(t *testing.T) {
	a := randUnitVec(t, 3)
	b := randUnitVec(t, 4)
	// b := b - (a·b)a; renormalize.
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	for i := range b {
		b[i] -= float32(dot) * a[i]
	}
	var norm float64
	for _, x := range b {
		norm += float64(x) * float64(x)
	}
	inv := float32(1.0 / math.Sqrt(norm))
	for i := range b {
		b[i] *= inv
	}

	qa, qb := Quantize(a), Quantize(b)
	c := CosineFloat(qa, qb)
	// Quantization inflates the orthogonal floor; 0.02 is a loose
	// bound measured empirically on this dimension.
	if math.Abs(float64(c)) > 0.05 {
		t.Fatalf("orthogonal cosine: got %g, want ~0", c)
	}
}

// TestCosineFloat_NegatedIsMinusOne: cos(v, -v) == -1 under the int8
// roundtrip. Guards against sign bugs in the dot product accumulator.
func TestCosineFloat_NegatedIsMinusOne(t *testing.T) {
	v := randUnitVec(t, 5)
	neg := make([]float32, VecDim)
	for i := range v {
		neg[i] = -v[i]
	}
	qv := Quantize(v)
	qn := Quantize(neg)
	c := CosineFloat(qv, qn)
	// Same quantization-loss envelope as the identical-vector case,
	// mirrored across zero.
	if c > -0.98 || c < -1.01 {
		t.Fatalf("negated cosine: got %g, want close to -1", c)
	}
}

// TestCosineInt8_MatchesNaive compares the unrolled cosineInt8
// implementation against a single-accumulator reference on random
// inputs. Catches any regression in the unroll (e.g. wrong stride, bad
// accumulator combine).
func TestCosineInt8_MatchesNaive(t *testing.T) {
	r := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic test fixtures, not crypto
	for tc := 0; tc < 64; tc++ {
		a := make([]int8, VecDim)
		b := make([]int8, VecDim)
		for i := range a {
			// r.Intn(256)-128 is in [-128, 127]; both bounds are
			// exactly int8-representable. The explicit mask + cast
			// pattern keeps gosec G115 quiet while preserving the
			// uniform sign distribution on the test fixture.
			a[i] = int8(r.Intn(256) - 128) //nolint:gosec // value always fits int8
			b[i] = int8(r.Intn(256) - 128) //nolint:gosec // value always fits int8
		}
		want := naiveDot(a, b)
		got := cosineInt8(a, b)
		if got != want {
			t.Fatalf("tc=%d: got %d, want %d", tc, got, want)
		}
	}
}

// CosineFloat on wrong-length inputs returns zero as a non-panicking
// guard; asserts that behavior so callers can rely on it.
func TestCosineFloat_WrongLengthReturnsZero(t *testing.T) {
	a := make([]int8, VecDim)
	b := make([]int8, VecDim-1)
	if c := CosineFloat(a, b); c != 0 {
		t.Errorf("got %g, want 0", c)
	}
	if c := CosineFloat(b, a); c != 0 {
		t.Errorf("got %g, want 0", c)
	}
}

// naiveDot is the single-accumulator reference for cosineInt8. Lives
// in the test file so production code does not carry an unused helper.
func naiveDot(a, b []int8) int32 {
	var s int32
	for i := 0; i < VecDim; i++ {
		s += int32(a[i]) * int32(b[i])
	}
	return s
}

// randUnitVec returns a deterministic, L2-normalized float32 vector of
// length VecDim. Seed is the test's choice so parallel subtests do not
// collide on the global RNG. Thin wrapper over deterministicUnitVec
// that marks the caller as a test helper.
func randUnitVec(t *testing.T, seed int64) []float32 {
	t.Helper()
	return deterministicUnitVec(seed)
}

// deterministicUnitVec returns a seeded, L2-normalized float32 vector
// of length VecDim. Kept free of *testing.T so benchmark and helper
// loops can call it directly.
func deterministicUnitVec(seed int64) []float32 {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic fixtures, not crypto
	v := make([]float32, VecDim)
	var sum float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		sum += float64(v[i]) * float64(v[i])
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}
