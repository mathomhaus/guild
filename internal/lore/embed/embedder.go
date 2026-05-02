// Package embed is a port-layer over an ONNX Runtime-backed sentence
// embedder used by lore's retrieval pipeline. It intentionally has zero
// imports from the rest of internal/lore so Phase 2 can swap the
// implementation (mathomhaus/ortpipe) without touching callers.
//
// On unix (darwin, linux) BGEEmbedder runs BAAI/bge-small-en-v1.5 via
// shota3506/onnxruntime-purego, loading libonnxruntime at runtime via
// purego.Dlopen. On windows the package ships only NullEmbedder, which
// returns (nil, ErrEmbedderDisabled) so callers can deterministically
// branch to BM25-only ranking (see ADR-003).
package embed

import (
	"context"
	"errors"
)

// Dim is the embedding dimension for BAAI/bge-small-en-v1.5 and its
// quantized variants. Every concrete Embedder in this package emits
// vectors of exactly this length (or returns an error).
const Dim = 384

// Embedder encodes a short text into a dense vector suitable for cosine
// similarity. Implementations must be safe for concurrent use.
//
// The return value is a float32 vector of length Dim; it is the caller's
// responsibility to quantize to int8 for on-disk storage (the quantization
// convention lives in the storage layer, not here, so this port stays
// numerics-pure for Phase 2).
type Embedder interface {
	// Embed encodes text into a Dim-length vector. Respects ctx cancellation
	// on the Go side, but ORT's Run cannot be cancelled mid-inference.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Dimension returns the embedding dimension. Always Dim for the
	// production BGE path; NullEmbedder also reports Dim so callers can
	// allocate receiving buffers without a nil check.
	Dimension() int
}

// Typed errors for caller-side branching. Production code should compare
// with errors.Is, not string matching.
var (
	// ErrEmbedderDisabled signals that this process has no working
	// embedder (Windows, dylib probe failed, feature-flag off, etc.).
	// Callers must fall through to BM25-only retrieval per ADR-003.
	ErrEmbedderDisabled = errors.New("embed: embedder disabled")

	// ErrEmbedderClosed signals that Embed was called on a Close'd
	// BGEEmbedder. Wrapping callers are expected to rebuild the
	// embedder rather than retry the same instance.
	ErrEmbedderClosed = errors.New("embed: session closed")

	// ErrUnexpectedOutputShape signals that the ONNX model's
	// last_hidden_state tensor did not have the expected
	// [1, seq_len, Dim] shape. Either the model or the port is wrong.
	ErrUnexpectedOutputShape = errors.New("embed: unexpected output shape")

	// ErrVocabMissingSpecial signals that the provided vocab.txt does
	// not contain one of the required BertTokenizer special tokens.
	// Fails loudly at construction rather than silently UNK-ing.
	ErrVocabMissingSpecial = errors.New("embed: vocab missing required special token")
)

// Compile-time assertions that every embedder in this package satisfies
// the Embedder interface. New implementations MUST appear here.
var (
	_ Embedder = (*NullEmbedder)(nil)
	_ Embedder = (*DeterministicEmbedder)(nil)
)
