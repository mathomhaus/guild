package embed

// Name-keyed embedder backend registry (ADR-006 Phase 4, section 4).
//
// This is Headroom's Protocol-ports + EXTERNAL/<backend_name> selection
// pattern (headroom/memory/factory.py, ports.py) expressed idiomatically in
// Go: an interface (Embedder, already defined in embedder.go) plus a
// map[string]EmbedderFactory populated by init(), with selection by config
// name. It mirrors internal/hooks/adapters/registry.go and the database/sql
// driver pattern: Register panics on programmer error (empty or duplicate
// name) because it only ever runs at init time.
//
// Parity contract (ADR-006 Phase 4): the registry is PURELY ADDITIVE. The
// default backend name (LocalBGEBackend / "" / "local-bge") resolves to the
// existing local BGE/ONNX construction path unchanged, so a default config
// yields byte-identical embeddings and wiring. Alternate backends only engage
// when [embed].backend names them explicitly.
//
// Placement: this lives in internal/lore/embed, not a new internal/backend
// package, because the package already owns the Embedder interface and has
// zero imports from the rest of internal/lore (see the package doc in
// embedder.go). Both internal/lore (WireEmbedDeps) and internal/mcp (the lazy
// providers) already import this package, so the registry is reachable from
// every embedder call site without a new edge or an import cycle.

import (
	"fmt"
	"sort"
	"sync"
)

// LocalBGEBackend is the stable registry name for guild's bundled local
// embedder: BAAI/bge-small-en-v1.5 run through onnxruntime-purego (the
// BGEEmbedder path). It is the default: an empty [embed].backend resolves
// here, and selecting it by name constructs exactly the embedder guild builds
// today. Do not rename without a config-compat migration.
const LocalBGEBackend = "local-bge"

// EmbedConfig is the backend-selection surface BuildEmbedder reads. It is a
// small value type local to this package so internal/config (which sits at the
// bottom of the dependency graph and must not import domain packages) is never
// pinned to internal/lore/embed: the adapter layer translates config.Config
// into an EmbedConfig at the call site. Zero value selects the default
// backend, preserving byte-identical behavior for a silent config.
type EmbedConfig struct {
	// Backend is the registry name selecting which embedder to build.
	// Empty string is normalized to LocalBGEBackend so a zero EmbedConfig
	// is the default local path. Unknown names are an error from
	// BuildEmbedder, never a silent fallback (a typo must be loud).
	Backend string

	// Model is the optional model identifier a backend factory may read
	// (e.g. an OpenAI/Ollama model name). The local BGE path ignores it:
	// its model is pinned by the bundled manifest. Empty is always valid.
	Model string
}

// EmbedderFactory constructs an Embedder for a given EmbedConfig. A factory
// must be safe to call once per process at wire time; it owns whatever native
// or network resources its Embedder needs. Returning an error makes
// BuildEmbedder surface it to the caller, which threads nil into EmbedDeps so
// the ADR-003 BM25-only fallback engages rather than crashing.
type EmbedderFactory func(cfg EmbedConfig) (Embedder, error)

var (
	embRegMu       sync.RWMutex
	embedderReg    = map[string]EmbedderFactory{}
	embedderClosed = map[string]func(Embedder){} // optional per-backend close hooks
)

// RegisterEmbedder adds factory under name to the global embedder registry. It
// panics when name is empty or already taken: both are bugs in the registering
// package, not runtime conditions, and Register only runs at init time where a
// panic is the right, loud failure. Call from an init() func:
//
//	func init() { embed.RegisterEmbedder("openai", newOpenAIEmbedder) }
func RegisterEmbedder(name string, factory EmbedderFactory) {
	if name == "" {
		panic("embed: RegisterEmbedder called with empty backend name")
	}
	if factory == nil {
		panic("embed: RegisterEmbedder called with nil factory for " + name)
	}
	embRegMu.Lock()
	defer embRegMu.Unlock()
	if _, dup := embedderReg[name]; dup {
		panic("embed: RegisterEmbedder called twice for backend " + name)
	}
	embedderReg[name] = factory
}

// BuildEmbedder selects the registered backend named by cfg.Backend (empty
// normalized to LocalBGEBackend) and constructs its Embedder. An unknown
// backend name returns an error listing the registered names so a typo is
// diagnosable, never silent.
//
// BuildEmbedder is the selection seam, NOT the local construction orchestrator.
// The default backend's factory deliberately produces the same BGEEmbedder the
// pre-Phase-4 path produced; WireEmbedDeps keeps owning the state gate, probe,
// warm-start, and index load for the default path so default behavior stays
// byte-identical. Callers select an ALTERNATE backend here and receive a bare
// Embedder to wrap themselves.
func BuildEmbedder(cfg EmbedConfig) (Embedder, error) {
	name := cfg.Backend
	if name == "" {
		name = LocalBGEBackend
	}
	embRegMu.RLock()
	factory, ok := embedderReg[name]
	embRegMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("embed: unknown embedder backend %q (registered: %v)", name, RegisteredEmbedders())
	}
	emb, err := factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("embed: build backend %q: %w", name, err)
	}
	return emb, nil
}

// IsDefaultBackend reports whether name selects the bundled local BGE path
// (empty or LocalBGEBackend). The default-path orchestrator (WireEmbedDeps)
// uses it to decide whether to run the existing byte-identical construction or
// to delegate to BuildEmbedder for an alternate backend.
func IsDefaultBackend(name string) bool {
	return name == "" || name == LocalBGEBackend
}

// RegisteredEmbedders returns every registered backend name, sorted, for
// error messages and tests. Allocates a fresh slice on each call.
func RegisteredEmbedders() []string {
	embRegMu.RLock()
	defer embRegMu.RUnlock()
	out := make([]string, 0, len(embedderReg))
	for n := range embedderReg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HasEmbedder reports whether a backend is registered under name (empty
// normalized to LocalBGEBackend). Lets a caller validate a configured backend
// name at load time before construction.
func HasEmbedder(name string) bool {
	if name == "" {
		name = LocalBGEBackend
	}
	embRegMu.RLock()
	_, ok := embedderReg[name]
	embRegMu.RUnlock()
	return ok
}
