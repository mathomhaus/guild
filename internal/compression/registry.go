package compression

import (
	"fmt"
	"sort"
	"sync"
)

// Name-keyed compressor-strategy registry (ADR-006 Phase 7), modeled on the
// Phase-4 embedder registry in internal/lore/embed/registry.go (which in turn
// mirrors internal/hooks/adapters and the database/sql driver pattern). Each
// compressor self-registers in an init() func:
//
//	func init() { compression.RegisterStrategy("diff", newDiffStrategy) }
//
// so importing the compression package makes every strategy reachable by
// name, and [compression].strategies selects which are live. Register panics
// on programmer error (empty or duplicate name) because it only runs at init
// time, where a panic is the right, loud failure.

// Result is the outcome of running a strategy over one blob of content.
type Result struct {
	// Compressed is the compact form to ship inline. For a lossless
	// strategy this is fully self-describing (no retrieval needed). For a
	// lossy-with-CCR strategy it carries a retrieval marker pointing at the
	// stashed original.
	Compressed string

	// Lossless reports whether Compressed alone reproduces the original.
	// True for the JSON/table compactor; false for diff/log/search, which
	// rely on the CCR store for full fidelity.
	Lossless bool

	// CacheKey is the CCR key under which the full original was stashed,
	// when the strategy engaged its lossy path. Empty when nothing was
	// stashed (lossless path, pass-through, or below the strategy's
	// minimum-size threshold).
	CacheKey string

	// OriginalBytes / CompressedBytes record the byte sizes so a caller can
	// report the savings without re-measuring.
	OriginalBytes   int
	CompressedBytes int
}

// Strategy is one named compressor. Compress receives the raw content, an
// optional query context (used for relevance scoring by strategies that
// rank/drop), and the CCR store to stash originals into (may be nil, in
// which case a lossy strategy still emits its marker but persists nothing —
// matching Headroom's store=None path).
type Strategy interface {
	// Name is the stable registry key and the value used in
	// [compression].strategies, e.g. "diff".
	Name() string
	// Lossless reports whether this strategy's output is self-contained
	// (no CCR retrieval required to reproduce the original).
	Lossless() bool
	// Compress runs the algorithm. A strategy that declines to compress
	// (input too small, unparseable) returns the input unchanged with an
	// empty CacheKey, never an error for ordinary content.
	Compress(content, context string, store Store) (Result, error)
}

// Factory constructs a Strategy. Kept as a factory (rather than a singleton)
// so a strategy can capture per-build config in the future; today every
// factory is a thin constructor.
type Factory func() Strategy

var (
	regMu      sync.RWMutex
	strategies = map[string]Factory{}
)

// RegisterStrategy adds factory under name to the global strategy registry.
// Panics on an empty name, a nil factory, or a duplicate name (all are bugs
// in the registering package, surfaced loudly at init time).
func RegisterStrategy(name string, factory Factory) {
	if name == "" {
		panic("compression: RegisterStrategy called with empty strategy name")
	}
	if factory == nil {
		panic("compression: RegisterStrategy called with nil factory for " + name)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := strategies[name]; dup {
		panic("compression: RegisterStrategy called twice for strategy " + name)
	}
	strategies[name] = factory
}

// BuildStrategy constructs the strategy registered under name. An unknown
// name returns an error listing the registered names so a typo is
// diagnosable, never silent.
func BuildStrategy(name string) (Strategy, error) {
	regMu.RLock()
	factory, ok := strategies[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("compression: unknown strategy %q (registered: %v)", name, RegisteredStrategies())
	}
	return factory(), nil
}

// HasStrategy reports whether a strategy is registered under name.
func HasStrategy(name string) bool {
	regMu.RLock()
	_, ok := strategies[name]
	regMu.RUnlock()
	return ok
}

// RegisteredStrategies returns every registered strategy name, sorted, for
// error messages and tests. Allocates a fresh slice each call.
func RegisteredStrategies() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(strategies))
	for n := range strategies {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
