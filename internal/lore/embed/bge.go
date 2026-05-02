// BGEEmbedder type and lifecycle. Unix-only because the ONNX Runtime
// loader (runtime_unix.go) uses purego.Dlopen, which is not available
// on purego's Windows surface (F11). Windows builds compile without
// this file and fall through to NullEmbedder via callers.

//go:build unix

package embed

import (
	"fmt"
	"math"
	"sync"
)

// BGEEmbedder is the production embedder path: BAAI/bge-small-en-v1.5
// run through shota3506/onnxruntime-purego with CLS-token pooling and
// L2 normalization. Matches the pooling config in bge-small's
// 1_Pooling/config.json (pooling_mode_cls_token=true) and the
// sentence-transformers Normalize step.
//
// F7: optimum-cli's ONNX export omits the pooling layer, so this code
// does the pool + normalize itself. Returning last_hidden_state[0]
// verbatim would yield semantically poor vectors.
//
// Safe for concurrent use: Embed guards the session with a mutex because
// ORT's CPU execution provider is not documented as concurrent-safe.
type BGEEmbedder struct {
	rt        *ortRuntime
	tokenizer *WordPieceTokenizer
	maxLen    int
	dim       int
	mu        sync.Mutex
}

// NewBGEEmbedder loads libonnxruntime at cfg.LibraryPath, opens the ONNX
// model at cfg.ModelPath, and attaches a WordPiece tokenizer driven by
// cfg.VocabPath. Caller must Close() the returned embedder when done.
//
// Every failure path releases the partially-constructed native
// resources before returning, so callers never leak a dylib handle.
func NewBGEEmbedder(cfg RuntimeConfig) (*BGEEmbedder, error) {
	if cfg.MaxLen <= 0 {
		cfg.MaxLen = 512
	}

	rt, err := openORTRuntime(cfg)
	if err != nil {
		return nil, err
	}

	vocab, err := LoadVocab(cfg.VocabPath)
	if err != nil {
		rt.close()
		return nil, fmt.Errorf("embed: LoadVocab(%q): %w", cfg.VocabPath, err)
	}
	tk := NewWordPieceTokenizer(vocab)
	if err := tk.assertVocabHasSpecials(); err != nil {
		rt.close()
		return nil, err
	}

	return &BGEEmbedder{
		rt:        rt,
		tokenizer: tk,
		maxLen:    cfg.MaxLen,
		dim:       Dim,
	}, nil
}

// Close releases the underlying ORT session, env, and runtime. Safe to
// call multiple times. After Close returns, Embed returns ErrEmbedderClosed.
func (e *BGEEmbedder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rt != nil {
		e.rt.close()
		e.rt = nil
	}
}

// Dimension returns the embedding dimension (always Dim for bge-small).
func (e *BGEEmbedder) Dimension() int { return e.dim }

// Compile-time proof that BGEEmbedder satisfies the Embedder interface.
// Lives in this unix-gated file because BGEEmbedder itself is unix-only.
var _ Embedder = (*BGEEmbedder)(nil)

// l2Normalize scales v to unit L2 norm in place. A zero-norm input is
// left untouched (callers get a zero vector back; the BGE model never
// emits exact zero output in practice but the guard keeps this pure).
// Uses float64 accumulation because summing 384 squared float32 values
// in float32 loses precision near the tail.
func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}
