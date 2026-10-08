package adapters

import (
	"testing"
)

// fakeAdapter is a minimal Adapter for registry plumbing tests.
type fakeAdapter struct{ name string }

func (f fakeAdapter) Name() string                { return f.name }
func (fakeAdapter) Detect() (bool, error)         { return false, nil }
func (fakeAdapter) SettingsPath() (string, error) { return "", nil }
func (fakeAdapter) Install(Config) error          { return nil }
func (fakeAdapter) Sync(Config) error             { return nil }
func (fakeAdapter) Scan() ([]Hook, error)         { return nil, nil }
func (fakeAdapter) Substitute(cmd string) string  { return cmd }

func TestRegisterLookupAll(t *testing.T) {
	Register(fakeAdapter{name: "zz-test-b"})
	Register(fakeAdapter{name: "aa-test-a"})

	if _, ok := Lookup("aa-test-a"); !ok {
		t.Error("Lookup(aa-test-a) not found after Register")
	}
	if _, ok := Lookup("never-registered"); ok {
		t.Error("Lookup(never-registered) found; want miss")
	}

	all := All()
	var names []string
	for _, a := range all {
		names = append(names, a.Name())
	}
	// All() is sorted by name; our two test adapters must appear in
	// alphabetical order relative to each other.
	posA, posB := -1, -1
	for i, n := range names {
		switch n {
		case "aa-test-a":
			posA = i
		case "zz-test-b":
			posB = i
		}
	}
	if posA < 0 || posB < 0 {
		t.Fatalf("All() missing test adapters: %v", names)
	}
	if posA > posB {
		t.Errorf("All() not sorted: %v", names)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	Register(fakeAdapter{name: "dup-test"})
	defer func() {
		if recover() == nil {
			t.Error("Register of duplicate name did not panic")
		}
	}()
	Register(fakeAdapter{name: "dup-test"})
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register with empty name did not panic")
		}
	}()
	Register(fakeAdapter{name: ""})
}
