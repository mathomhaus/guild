// Probe-layer tests. Exercises RunProbe with canned embedders so
// neither ORT nor libonnxruntime needs to be present.

package embed

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestRunProbe_NullEmbedderFails ensures the short-circuit: a
// NullEmbedder's error bubbles up as probe err (not a silent pass).
func TestRunProbe_NullEmbedderFails(t *testing.T) {
	res := RunProbe(context.Background(), NewNullEmbedder())
	if res.Err == nil {
		t.Fatal("probe against null embedder returned no error")
	}
	if !errors.Is(res.Err, ErrEmbedderDisabled) {
		t.Errorf("want err to wrap ErrEmbedderDisabled, got %v", res.Err)
	}
}

// TestRunProbe_ReferenceMatchesItself: reading the reference vector
// and feeding it through the embedding-verifier path proves the
// cosine + JSON + fixture wiring are intact. A fake embedder that
// returns the exact reference bytes for ProbeString must pass.
func TestRunProbe_ReferenceMatchesItself(t *testing.T) {
	// Load the reference embedding for ProbeString.
	var m map[string]struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(referenceVectorsJSON, &m); err != nil {
		t.Fatalf("unmarshal reference: %v", err)
	}
	ref, ok := m[ProbeString]
	if !ok {
		t.Fatalf("reference missing for %q", ProbeString)
	}
	if len(ref.Embedding) != Dim {
		t.Fatalf("reference dim %d, want %d", len(ref.Embedding), Dim)
	}
	fake := &constVecEmbedder{vec: ref.Embedding}
	res := RunProbe(context.Background(), fake)
	if res.Err != nil {
		t.Fatalf("probe err: %v", res.Err)
	}
	if res.Cosine < ProbeMinCosine {
		t.Errorf("cosine %.6f below floor %.6f", res.Cosine, ProbeMinCosine)
	}
	if res.Dim != Dim {
		t.Errorf("dim %d want %d", res.Dim, Dim)
	}
}

// TestRunProbe_MismatchFails: an orthogonal vector must fail the
// probe with ErrProbeMismatch.
func TestRunProbe_MismatchFails(t *testing.T) {
	// Build a simple unit vector in dim 0 direction.
	vec := make([]float32, Dim)
	vec[0] = 1
	fake := &constVecEmbedder{vec: vec}
	res := RunProbe(context.Background(), fake)
	if res.Err == nil {
		t.Fatal("expected err on mismatched vector")
	}
	if !errors.Is(res.Err, ErrProbeMismatch) {
		t.Errorf("want ErrProbeMismatch, got %v", res.Err)
	}
}

// constVecEmbedder returns a fixed vector for every input. Test-only.
type constVecEmbedder struct {
	vec []float32
}

func (c *constVecEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	// return a copy so callers can't mutate the template.
	out := make([]float32, len(c.vec))
	copy(out, c.vec)
	return out, nil
}

func (c *constVecEmbedder) Dimension() int { return len(c.vec) }
