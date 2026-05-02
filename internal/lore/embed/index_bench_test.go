package embed

import (
	"math/rand"
	"testing"
)

// Benchmarks for the index cosine hot path.
//
// Run:
//
//	go test -bench=. -benchmem -run=^$ ./internal/lore/embed/
//
// Reference numbers (Apple M3 Pro, 11 cores, 18 GB, Darwin 24.6.0
// arm64, Go 1.25, -benchtime=2s):
//
//	BenchmarkIndex_TopK_10k-11      1285    1.85 ms/op   164 KiB/op  4 allocs/op
//	BenchmarkIndex_TopK_1k-11      15884    0.16 ms/op    16 KiB/op  4 allocs/op
//	BenchmarkIndex_TopK_100k-11      100    22.8 ms/op   1.6 MiB/op  4 allocs/op
//	BenchmarkCosineInt8-11      25238770      98 ns/op       0 B/op   0 allocs/op
//
// The 10k p50 is ~1.85 ms, comfortably under the 5 ms bar on LORE-358
// and the ADR-003 projection. If this regresses materially on newer
// Go releases or smaller CPUs, ADR-003's follow-up is an in-memory
// BLAS-backed implementation; we are not there yet.
//
// The 100k datapoint confirms the ADR-003 note that sqlite-vec starts
// to earn its keep around that corpus size: at ~23 ms per query, a
// scan-per-call design is still usable but no longer free.
//
// These benches live in index_bench_test.go per convention; `make
// check` runs `go test -race` without -bench so benchmarks do not
// slow down the gate. CI invokes -bench=. separately when the
// perf-regression guard is added.

// BenchmarkIndex_TopK_10k measures TopK at the canonical corpus size
// and k=60 documented in ADR-003. Index is built once per benchmark
// (outside the timed region) so the loop body measures only the
// cosine scan + top-k sort.
func BenchmarkIndex_TopK_10k(b *testing.B) {
	benchTopK(b, 10_000, 60)
}

// BenchmarkIndex_TopK_1k gives a small-corpus datapoint so regressions
// that scale with n are obvious from the 1k/10k ratio.
func BenchmarkIndex_TopK_1k(b *testing.B) {
	benchTopK(b, 1_000, 60)
}

// BenchmarkIndex_TopK_100k projects past the ADR-003 horizon: the ADR
// notes sqlite-vec starts earning its keep around 100k entries, and
// this bench answers "by how much" on the target hardware. Not part
// of the acceptance criteria; kept for curiosity and early warning.
func BenchmarkIndex_TopK_100k(b *testing.B) {
	if testing.Short() {
		b.Skip("100k bench skipped under -short")
	}
	benchTopK(b, 100_000, 60)
}

// BenchmarkCosineInt8 measures the inner loop alone so future
// micro-optimizations (SIMD via assembly, fewer loads) can be
// attributed to the inner kernel vs. the surrounding TopK machinery.
func BenchmarkCosineInt8(b *testing.B) {
	r := rand.New(rand.NewSource(1)) //nolint:gosec // fixture RNG
	a := make([]int8, VecDim)
	c := make([]int8, VecDim)
	for i := range a {
		// r.Intn(256)-128 is always in [-128, 127], which fits int8
		// exactly; gosec G115 cannot prove that statically.
		a[i] = int8(r.Intn(256) - 128) //nolint:gosec // value always fits int8
		c[i] = int8(r.Intn(256) - 128) //nolint:gosec // value always fits int8
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sum int32
	for i := 0; i < b.N; i++ {
		sum += cosineInt8(a, c)
	}
	// Keep the sum live; prevents the compiler from eliding the call.
	b.StopTimer()
	if sum == 0x7fffffff {
		b.Fatalf("unreachable sentinel: %d", sum)
	}
}

// benchTopK builds a deterministic int8 corpus of size n, then runs
// TopK(k) in the timed region. Splice is used rather than LoadFromDB
// so the benchmark does not depend on a live SQLite instance.
func benchTopK(b *testing.B, n, k int) {
	b.Helper()
	idx := buildBenchIndex(b, n)
	qvec := Quantize(deterministicUnitVec(int64(n) + 7))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := idx.TopK(qvec, k)
		if err != nil {
			b.Fatalf("TopK: %v", err)
		}
		if len(hits) != k {
			b.Fatalf("len(hits) = %d, want %d", len(hits), k)
		}
	}
}

// buildBenchIndex populates an Index with n deterministic int8 vectors
// via Splice. Keeps the bench setup standalone (no DB, no embedder).
func buildBenchIndex(b *testing.B, n int) *Index {
	b.Helper()
	idx := NewIndex(LoreCorpus{}, canonModelID)
	// Splice requires a loaded index; a zero-row LoadFromDB would need
	// a live DB. Seed the loaded flag by splicing the first vector
	// after cosmetically "loading" via an empty Splice path.
	// Because Splice flips loaded=true on first call, this just works.
	for i := 0; i < n; i++ {
		v := Quantize(deterministicUnitVec(int64(i + 1)))
		if err := idx.Splice(int64(i+1), v, int64(i+1)); err != nil {
			b.Fatalf("Splice: %v", err)
		}
	}
	return idx
}
