package embed

// Local BGE backend registration (ADR-006 Phase 4, deliverable 2).
//
// Registers the bundled BAAI/bge-small-en-v1.5 ONNX embedder under the stable
// name LocalBGEBackend ("local-bge"), with "bge" and "local" as aliases, so it
// is selectable by [embed].backend exactly like an alternate backend. This is
// what makes the registry's selection path uniform: the default is just one
// more named entry, not a special case at the call site.
//
// CRITICAL parity note: this factory is NOT on the byte-identical default hot
// path. WireEmbedDeps (internal/lore) keeps owning the default construction
// (state gate, warm-start fast path, PrepareAndProbe, index load, identity
// meta) verbatim; it only delegates to BuildEmbedder when an ALTERNATE backend
// is named (see IsDefaultBackend). This factory exists so the default backend
// is a first-class registry citizen and so the selection test can prove that
// asking for "local-bge" by name yields the local BGE embedder. The Embedder
// it constructs goes through the SAME extract + newBGEEmbedderFromExt
// primitives the default path uses, so when the factory IS exercised it
// produces the identical embedder type and vectors.

import (
	"fmt"
)

func init() {
	// One factory, three names. Aliases keep the config ergonomic
	// ("backend = bge") without a second implementation. Registering the
	// same factory under several names is intentional and cheap.
	RegisterEmbedder(LocalBGEBackend, newLocalBGEEmbedder)
	RegisterEmbedder("bge", newLocalBGEEmbedder)
	RegisterEmbedder("local", newLocalBGEEmbedder)
}

// newLocalBGEEmbedder constructs the bundled local BGE/ONNX embedder from the
// extracted cache assets. It mirrors WarmStartEmbedder's construction (Extract
// then newBGEEmbedderFromExt) so the embedder it returns is the same concrete
// BGEEmbedder the default path builds. cfg.Model is ignored: the local model
// is pinned by the bundled manifest, not chosen by config.
//
// On a build without bundled assets (default, no -tags=withembed) or on a
// platform where the BGE path cannot link (Windows), it returns an error so
// BuildEmbedder surfaces it and the caller falls back to BM25-only retrieval
// per ADR-003. This matches the default path's "no_bundled_assets" /
// "platform_disabled" outcomes.
func newLocalBGEEmbedder(_ EmbedConfig) (Embedder, error) {
	man := CurrentManifest()
	if !HasAssets() || !man.hasAssetBytes() {
		return nil, fmt.Errorf("embed: local-bge backend: %w", ErrNoAssets)
	}
	cacheDir, err := ResolveCacheDir(man)
	if err != nil {
		return nil, fmt.Errorf("embed: local-bge backend: resolve cache dir: %w", err)
	}
	ext, err := Extract(man, cacheDir)
	if err != nil {
		return nil, fmt.Errorf("embed: local-bge backend: extract: %w", err)
	}
	// newBGEEmbedderFromExt is the same unix-gated constructor the default
	// path (PrepareAndProbe, WarmStartEmbedder) uses; on non-unix it returns
	// ErrEmbedderDisabled, which we surface as the factory error.
	emb, _, err := newBGEEmbedderFromExt(ext)
	if err != nil {
		return nil, fmt.Errorf("embed: local-bge backend: init: %w", err)
	}
	return emb, nil
}
