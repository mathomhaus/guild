package adapters

// Self-registration plumbing. Each adapter package registers itself in
// an init() func:
//
//	func init() { adapters.Register(Adapter{}) }
//
// so that importing the adapter package (typically from cmd/guild or a
// test) is all it takes to make the adapter visible to the `guild
// hooks` CLI via All / Lookup. This mirrors the database/sql driver
// pattern: Register panics on programmer error (empty or duplicate
// name) because it only ever runs at init time.

import (
	"sort"
	"sync"
)

var (
	regMu    sync.Mutex
	registry = map[string]Adapter{}
)

// Register adds a to the global adapter registry. It panics when the
// name is empty or already taken: both are bugs in the registering
// package, not runtime conditions.
func Register(a Adapter) {
	name := a.Name()
	if name == "" {
		panic("hooks/adapters: Register called with empty adapter name")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("hooks/adapters: Register called twice for adapter " + name)
	}
	registry[name] = a
}

// All returns every registered adapter, sorted by name for stable CLI
// output.
func All() []Adapter {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Adapter, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Lookup returns the adapter registered under name, if any.
func Lookup(name string) (Adapter, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	a, ok := registry[name]
	return a, ok
}
