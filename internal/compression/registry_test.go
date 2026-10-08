package compression

import (
	"sort"
	"testing"

	"github.com/mathomhaus/guild/internal/module"
)

func TestRegisteredStrategies(t *testing.T) {
	got := RegisteredStrategies()
	want := []string{"diff", "json", "log", "search"}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("registered strategies = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registered strategies = %v, want %v", got, want)
		}
	}
	for _, name := range want {
		if !HasStrategy(name) {
			t.Errorf("HasStrategy(%q) = false", name)
		}
		s, err := BuildStrategy(name)
		if err != nil {
			t.Errorf("BuildStrategy(%q): %v", name, err)
			continue
		}
		if s.Name() != name {
			t.Errorf("strategy %q reports Name() = %q", name, s.Name())
		}
	}
}

func TestBuildUnknownStrategyErrors(t *testing.T) {
	if _, err := BuildStrategy("nope"); err == nil {
		t.Fatal("BuildStrategy of unknown name should error")
	}
}

func TestStrategyLosslessFlags(t *testing.T) {
	cases := map[string]bool{"json": true, "diff": false, "log": false, "search": false}
	for name, lossless := range cases {
		s, err := BuildStrategy(name)
		if err != nil {
			t.Fatal(err)
		}
		if s.Lossless() != lossless {
			t.Errorf("%s Lossless() = %v, want %v", name, s.Lossless(), lossless)
		}
	}
}

// TestModuleRegisteredOffByDefault verifies the compression module is in the
// registry but DefaultEnabled() is false, so the default-config Enabled set
// (nil predicate) excludes it entirely.
func TestModuleRegisteredOffByDefault(t *testing.T) {
	m, ok := module.Lookup("compression")
	if !ok {
		t.Fatal("compression module not registered")
	}
	if m.DefaultEnabled() {
		t.Fatal("compression module must default to disabled")
	}
	// With a nil predicate (every module on its own default), compression is
	// absent from Enabled.
	for _, em := range module.Enabled(nil) {
		if em.Name() == "compression" {
			t.Fatal("compression must be absent from the default Enabled set")
		}
	}
	// With an explicit enable predicate, it appears with both verbs.
	enabled := module.Enabled(func(name string, def bool) bool {
		if name == "compression" {
			return true
		}
		return def
	})
	found := false
	for _, em := range enabled {
		if em.Name() != "compression" {
			continue
		}
		found = true
		if len(em.Commands()) != 2 {
			t.Errorf("compression should contribute 2 commands, got %d", len(em.Commands()))
		}
		if fsys, db := em.Migrations(); fsys != nil || db != "" {
			t.Error("compression owns no database")
		}
		if len(em.Services()) != 0 {
			t.Error("compression runs no daemon services")
		}
		if em.Instructions() != "" {
			t.Error("compression Instructions() must be \"\" to keep the contract byte-identical")
		}
	}
	if !found {
		t.Fatal("compression should be present when explicitly enabled")
	}
}
