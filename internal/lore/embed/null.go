package embed

import "context"

// NullEmbedder is the always-present fallback. It never produces a
// semantic vector; every call returns ErrEmbedderDisabled so callers
// deterministically branch to BM25-only retrieval (ADR-003, F18: a zero
// vector silently pollutes RRF fusion and must never be returned as a
// success).
//
// Windows installs and any unix install where dylib load or probe fails
// get this embedder; lore_appraise then serves BM25+stopwords per the
// deterministic-fallback contract.
type NullEmbedder struct {
	// dim is reported via Dimension so callers can size receiving
	// buffers even when the embedder never produces values.
	dim int
}

// NewNullEmbedder constructs a NullEmbedder reporting the package's
// canonical Dim. Accepting no config keeps the caller surface trivial.
func NewNullEmbedder() *NullEmbedder {
	return &NullEmbedder{dim: Dim}
}

// Embed always returns (nil, ErrEmbedderDisabled). Context cancellation
// is still honored: a cancelled context takes precedence so callers
// testing cancellation see ctx.Err() first.
func (e *NullEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrEmbedderDisabled
}

// Dimension returns the dimension the fallback reports. Always Dim.
func (e *NullEmbedder) Dimension() int {
	if e.dim <= 0 {
		return Dim
	}
	return e.dim
}
