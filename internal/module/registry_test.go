package module

import (
	"io/fs"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
)

// reset clears the global registry between tests. Module registration is a
// process-init concern in production and is never torn down there, so this
// helper lives in the test file only.
func reset() {
	regMu.Lock()
	defer regMu.Unlock()
	registry = map[string]Module{}
}

// fakeModule is a minimal Module used by the registry tests.
type fakeModule struct {
	name string
	def  bool
}

func (f fakeModule) Name() string                            { return f.name }
func (f fakeModule) DefaultEnabled() bool                    { return f.def }
func (f fakeModule) Commands() []command.Registrant          { return nil }
func (f fakeModule) Migrations() (fsys fs.FS, dbName string) { return nil, "" }
func (f fakeModule) Services() []Service                     { return nil }
func (f fakeModule) Instructions() string                    { return "" }

func TestRegisterAndLookup(t *testing.T) {
	reset()
	Register(fakeModule{name: "alpha", def: true})

	m, ok := Lookup("alpha")
	if !ok || m.Name() != "alpha" {
		t.Fatalf("Lookup(alpha) = %v, %v; want the alpha module", m, ok)
	}
	if _, ok := Lookup("missing"); ok {
		t.Errorf("Lookup(missing) ok = true; want false")
	}
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	reset()
	defer func() {
		if recover() == nil {
			t.Errorf("Register with empty name did not panic")
		}
	}()
	Register(fakeModule{name: ""})
}

func TestRegisterDuplicatePanics(t *testing.T) {
	reset()
	Register(fakeModule{name: "dup", def: true})
	defer func() {
		if recover() == nil {
			t.Errorf("duplicate Register did not panic")
		}
	}()
	Register(fakeModule{name: "dup", def: true})
}

func TestAllSortedByName(t *testing.T) {
	reset()
	Register(fakeModule{name: "gamma"})
	Register(fakeModule{name: "alpha"})
	Register(fakeModule{name: "beta"})

	got := names(All())
	want := []string{"alpha", "beta", "gamma"}
	if !equalStrings(got, want) {
		t.Errorf("All() names = %v, want %v", got, want)
	}
}

func TestEnabledNilPredicateUsesDefaults(t *testing.T) {
	reset()
	Register(fakeModule{name: "on", def: true})
	Register(fakeModule{name: "off", def: false})

	got := names(Enabled(nil))
	want := []string{"on"}
	if !equalStrings(got, want) {
		t.Errorf("Enabled(nil) = %v, want %v (defaults only)", got, want)
	}
}

func TestEnabledPredicateOverridesBothDirections(t *testing.T) {
	reset()
	Register(fakeModule{name: "core", def: true})   // default on -> force off
	Register(fakeModule{name: "extra", def: false}) // default off -> force on

	seenDef := map[string]bool{}
	pred := func(name string, def bool) bool {
		seenDef[name] = def
		switch name {
		case "core":
			return false
		case "extra":
			return true
		default:
			return def
		}
	}

	got := names(Enabled(pred))
	want := []string{"extra"}
	if !equalStrings(got, want) {
		t.Errorf("Enabled(pred) = %v, want %v", got, want)
	}
	// The predicate must receive each module's own DefaultEnabled value.
	if seenDef["core"] != true || seenDef["extra"] != false {
		t.Errorf("predicate saw defaults %v, want core=true extra=false", seenDef)
	}
}

func names(ms []Module) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name()
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
