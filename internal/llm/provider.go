// Package llm is the name-keyed LLM provider/model selection seam (ADR-006
// Phase 4, deliverable 5).
//
// It establishes the config surface + registry that a FUTURE LLM-calling
// module (e.g. a sleep/compression module that summarizes or compresses
// context) will use to pick a provider and model by name, exactly the way
// internal/lore/embed picks an embedder backend. This package adds NO live LLM
// dependency and NO network client: it is the interface, the registry, and one
// registered no-op stub. The whole point is to land the seam now so a later
// module is "implement Provider + blank-import + a [provider] stanza" instead
// of reshaping config and wiring.
//
// Design: an interface (Provider) plus a map[string]ProviderFactory populated
// by init(), with selection by config name. This mirrors
// internal/hooks/adapters/registry.go, internal/module/registry.go, and the
// embedder registry, and the database/sql driver pattern: Register panics on
// programmer error (empty/duplicate/nil), BuildProvider resolves by name and
// errors loudly on an unknown name.
//
// Why a separate package (not internal/lore/embed): an LLM provider is a
// different capability than a sentence embedder (chat/completion vs dense
// vectors) and a future provider implementation will pull a real SDK; isolating
// it keeps that dependency off the embedder package and off the kernel until a
// module opts in. internal/config does not import this package (it stays at the
// bottom of the graph); the future module's wiring translates config.Provider
// into a ProviderConfig at its call site, the same edge the embedder seam uses.
package llm

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// NoopProvider is the stable name of the default no-op provider: it makes no
// network call and returns a deterministic stub completion. It is the default
// so a silent config never reaches a real LLM. Do not rename without a
// config-compat migration.
const NoopProvider = "noop"

// ProviderConfig is the selection surface BuildProvider reads. It is a small
// value type local to this package so internal/config is never pinned to
// internal/llm; the future LLM-calling module translates config.ProviderConfig
// into this at its wiring call site. Zero value selects the default no-op
// provider.
type ProviderConfig struct {
	// Backend is the registry name selecting the provider. Empty is
	// normalized to NoopProvider. Unknown names are an error from
	// BuildProvider, never a silent fallback.
	Backend string

	// Model is the model identifier handed to the provider (e.g. a chat
	// model name). Empty defers to the provider's own default. The no-op
	// provider ignores it.
	Model string
}

// CompletionRequest is the minimal request shape the seam carries. It is
// intentionally small: a future real provider can extend its own internal
// request type; this is only enough for the registry contract and the no-op
// stub to be exercised by a test. No streaming, tools, or message roles are
// modeled here, by design (the seam is minimal per the ADR).
type CompletionRequest struct {
	// Prompt is the single text input. A future provider may map this onto a
	// richer message list internally.
	Prompt string
	// Model overrides the provider's configured model for this one call.
	// Empty uses the provider's configured/default model.
	Model string
}

// CompletionResponse is the minimal response shape. A future real provider
// fills Text from its API; the no-op stub returns a deterministic marker.
type CompletionResponse struct {
	// Text is the completion output.
	Text string
	// Model is the model the provider reports it used.
	Model string
}

// Provider is a name-selected LLM backend. Implementations must be safe for
// concurrent use. The interface is deliberately tiny (one call) so landing the
// seam commits guild to almost nothing; a real provider extends its own
// surface behind this contract.
type Provider interface {
	// Name returns the provider's registry name, e.g. "noop".
	Name() string
	// Complete runs one completion. Implementations honor ctx cancellation.
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// ProviderFactory constructs a Provider for a given ProviderConfig. Called once
// per process at wire time by BuildProvider.
type ProviderFactory func(cfg ProviderConfig) (Provider, error)

var (
	provRegMu   sync.RWMutex
	providerReg = map[string]ProviderFactory{}
)

// RegisterProvider adds factory under name to the global provider registry. It
// panics on empty name, nil factory, or a duplicate name: all are init-time
// programmer errors. Call from an init() func.
func RegisterProvider(name string, factory ProviderFactory) {
	if name == "" {
		panic("llm: RegisterProvider called with empty provider name")
	}
	if factory == nil {
		panic("llm: RegisterProvider called with nil factory for " + name)
	}
	provRegMu.Lock()
	defer provRegMu.Unlock()
	if _, dup := providerReg[name]; dup {
		panic("llm: RegisterProvider called twice for provider " + name)
	}
	providerReg[name] = factory
}

// BuildProvider selects the registered provider named by cfg.Backend (empty
// normalized to NoopProvider) and constructs it. An unknown name returns an
// error listing the registered names.
func BuildProvider(cfg ProviderConfig) (Provider, error) {
	name := cfg.Backend
	if name == "" {
		name = NoopProvider
	}
	provRegMu.RLock()
	factory, ok := providerReg[name]
	provRegMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider backend %q (registered: %v)", name, RegisteredProviders())
	}
	p, err := factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("llm: build provider %q: %w", name, err)
	}
	return p, nil
}

// IsDefaultProvider reports whether name selects the default no-op provider
// (empty or NoopProvider).
func IsDefaultProvider(name string) bool {
	return name == "" || name == NoopProvider
}

// RegisteredProviders returns every registered provider name, sorted.
func RegisteredProviders() []string {
	provRegMu.RLock()
	defer provRegMu.RUnlock()
	out := make([]string, 0, len(providerReg))
	for n := range providerReg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HasProvider reports whether a provider is registered under name (empty
// normalized to NoopProvider).
func HasProvider(name string) bool {
	if name == "" {
		name = NoopProvider
	}
	provRegMu.RLock()
	_, ok := providerReg[name]
	provRegMu.RUnlock()
	return ok
}
