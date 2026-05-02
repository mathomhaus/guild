// Probe: a pinned "does the embedder produce the right numbers" check.
// guild init runs this once after extraction; a pass flips
// meta.embedder_state to 'enabled', a fail leaves it 'disabled' with a
// structured reason in the logs.
//
// The probe string and its reference vector are pinned (see
// ProbeString constant and the reference_vectors.json fixture shipped
// with the embedder package). Changing either invalidates the reference
// value and requires a spike-style re-run to regenerate numbers.

package embed

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// ProbeString is the pinned input used by the probe. Chosen from the
// reference_vectors.json fixture so the fixture already contains the
// reference embedding.
const ProbeString = "retry logic with exponential backoff"

// ProbeMinCosine is the acceptance floor. Matches parity_test.go so a
// passing probe implies a passing parity suite within numeric drift.
const ProbeMinCosine = 0.999

// ErrProbeUnavailable signals the probe could not run because there is
// no reference vector in the fixture for ProbeString. Only returned from
// misconfiguration (fixture surgery); production code should not hit
// this path.
var ErrProbeUnavailable = errors.New("embed: probe reference vector unavailable")

// ErrProbeMismatch signals the embedder produced a vector that cosine-
// differed from the reference by more than the allowed drift. Callers
// surface this as structured disabled-reason in the init logs.
var ErrProbeMismatch = errors.New("embed: probe vector cosine below floor")

//go:embed testdata/reference_vectors.json
var referenceVectorsJSON []byte

// ProbeResult describes the outcome of one probe run. Returned by
// RunProbe for structured logging. Cosine is 0 when the probe could not
// run; callers check Err first.
type ProbeResult struct {
	// Cosine is the cosine similarity between the embedder output and
	// the pinned reference vector, in [-1, 1]. NaN-free (callers can
	// log it directly without formatting guards).
	Cosine float64
	// Dim is the vector length returned by the embedder. Logged for
	// the "unexpected-dim" failure mode.
	Dim int
	// Floor is the acceptance threshold the probe compared against.
	// Logged alongside Cosine so a failure line is self-contained
	// without requiring the reader to know ProbeMinCosine.
	Floor float64
	// Err is nil on success and non-nil on any failure. Success means
	// Cosine >= ProbeMinCosine and Dim == Dim (package constant).
	Err error
}

// RunProbe encodes ProbeString with e, loads the pinned reference from
// the embedded testdata/reference_vectors.json fixture, and compares
// cosine similarity. Returns a ProbeResult; callers look at Err for
// pass/fail and log Cosine for structured context.
//
// Takes ctx so hung ORT sessions do not block init indefinitely; the
// caller is expected to derive a bounded context (5s in guild init).
// Context cancellation during Embed is honored on the Go side; ORT's
// forward pass itself is uncancellable.
func RunProbe(ctx context.Context, e Embedder) ProbeResult {
	if e == nil {
		return ProbeResult{Floor: ProbeMinCosine, Err: fmt.Errorf("embed: RunProbe: nil embedder")}
	}
	ref, err := loadReferenceEmbedding(ProbeString)
	if err != nil {
		return ProbeResult{Floor: ProbeMinCosine, Err: err}
	}
	got, err := e.Embed(ctx, ProbeString)
	if err != nil {
		return ProbeResult{Floor: ProbeMinCosine, Err: fmt.Errorf("embed: RunProbe: embed: %w", err)}
	}
	if len(got) != Dim {
		return ProbeResult{Dim: len(got), Floor: ProbeMinCosine, Err: fmt.Errorf("embed: RunProbe: got dim=%d want %d", len(got), Dim)}
	}
	if len(ref) != Dim {
		return ProbeResult{Dim: len(got), Floor: ProbeMinCosine, Err: fmt.Errorf("embed: RunProbe: reference dim=%d want %d", len(ref), Dim)}
	}
	cos := cosineSimilarity(got, ref)
	if math.IsNaN(cos) || math.IsInf(cos, 0) {
		return ProbeResult{Dim: len(got), Floor: ProbeMinCosine, Err: fmt.Errorf("embed: RunProbe: non-finite cosine")}
	}
	if cos < ProbeMinCosine {
		return ProbeResult{Cosine: cos, Dim: len(got), Floor: ProbeMinCosine, Err: fmt.Errorf("%w: got %.6f want >= %.6f", ErrProbeMismatch, cos, ProbeMinCosine)}
	}
	return ProbeResult{Cosine: cos, Dim: len(got), Floor: ProbeMinCosine}
}

// loadReferenceEmbedding parses the embedded reference_vectors.json and
// returns the embedding for text (or ErrProbeUnavailable).
func loadReferenceEmbedding(text string) ([]float32, error) {
	var m map[string]struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(referenceVectorsJSON, &m); err != nil {
		return nil, fmt.Errorf("embed: parse reference_vectors.json: %w", err)
	}
	entry, ok := m[text]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrProbeUnavailable, text)
	}
	if len(entry.Embedding) == 0 {
		return nil, fmt.Errorf("%w: %q: empty embedding", ErrProbeUnavailable, text)
	}
	return entry.Embedding, nil
}

// cosineSimilarity returns dot(a,b) / (||a|| * ||b||). float64
// accumulation because float32 dot of 384 terms loses precision at the
// tail. Returns 0 on degenerate zero-norm input (never NaN).
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
