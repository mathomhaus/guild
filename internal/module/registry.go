package module

import (
	"sort"
	"sync"
)

// Self-registration plumbing, modeled on internal/hooks/adapters/registry.go
// (which in turn mirrors the database/sql driver pattern). Each module
// registers itself in an init() func:
//
//	func init() { module.Register(loreModule{}) }
//
// so that importing the module package (from cmd/guild or a test) makes it
// visible to the kernel via All / Enabled / Lookup. Register panics on
// programmer error (empty or duplicate name) because it only ever runs at
// init time, where a panic is the right, loud failure.

var (
	regMu    sync.Mutex
	registry = map[string]Module{}
)

// Register adds m to the global module registry. It panics when the name is
// empty or already taken: both are bugs in the registering package, not
// runtime conditions.
func Register(m Module) {
	name := m.Name()
	if name == "" {
		panic("module: Register called with empty module name")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("module: Register called twice for module " + name)
	}
	registry[name] = m
}

// All returns every registered module, sorted by name for deterministic
// wiring order (stable tool/verb registration, stable instruction-fragment
// concatenation).
func All() []Module {
	regMu.Lock()
	defer regMu.Unlock()
	return sortedLocked()
}

// Lookup returns the module registered under name, if any.
func Lookup(name string) (Module, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	m, ok := registry[name]
	return m, ok
}

// Enabled returns the registered modules the operator has left active, in
// deterministic (name-sorted) order. isEnabled decides each module's fate:
// it receives the module name and the module's own DefaultEnabled() value
// and returns the final verdict, so config can override the default in
// either direction. Passing a nil isEnabled treats every module as enabled
// per its own default.
//
// The predicate indirection (rather than taking a *config.Config directly)
// keeps this package decoupled from internal/config and trivially testable;
// Phase 3 supplies a predicate backed by the [modules] table.
func Enabled(isEnabled func(name string, def bool) bool) []Module {
	regMu.Lock()
	all := sortedLocked()
	regMu.Unlock()

	out := make([]Module, 0, len(all))
	for _, m := range all {
		def := m.DefaultEnabled()
		on := def
		if isEnabled != nil {
			on = isEnabled(m.Name(), def)
		}
		if on {
			out = append(out, m)
		}
	}
	return out
}

// sortedLocked returns the registry's modules sorted by name. Callers must
// hold regMu.
func sortedLocked() []Module {
	out := make([]Module, 0, len(registry))
	for _, m := range registry {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
