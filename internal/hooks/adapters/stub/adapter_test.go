package stub

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
)

// TestStub_SelfRegisters: importing this package is enough to make the
// adapter visible through the registry (the init() side effect).
func TestStub_SelfRegisters(t *testing.T) {
	a, ok := adapters.Lookup("stub")
	if !ok {
		t.Fatal("stub adapter not found in registry after import")
	}
	if a.Name() != "stub" {
		t.Errorf("Name() = %q; want %q", a.Name(), "stub")
	}
}

// TestStub_InstallSyncScanRoundTrip exercises the full adapter contract
// against a hermetic home: install writes the rendered base config,
// scan reads it back, a second sync is a byte-level no-op.
func TestStub_InstallSyncScanRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	a, ok := adapters.Lookup("stub")
	if !ok {
		t.Fatal("stub adapter not registered")
	}
	base := hooks.DefaultBase()

	if err := a.Install(base); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path, err := a.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".guild", "stub-settings.json"); path != want {
		t.Errorf("SettingsPath = %q; want %q", path, want)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings file not written: %v", err)
	}

	scanned, err := a.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(scanned) != 3 {
		t.Fatalf("Scan returned %d hooks; want 3", len(scanned))
	}
	for _, h := range scanned {
		if !h.GuildOwned {
			t.Errorf("hook %q not guild-owned after clean install", h.Command)
		}
	}

	// Second sync: idempotent, no rewrite.
	if err := a.Sync(base); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second Sync rewrote the file:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestStub_ScanMissingFileIsEmpty: a never-installed harness scans to
// an empty list, not an error.
func TestStub_ScanMissingFileIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a, ok := adapters.Lookup("stub")
	if !ok {
		t.Fatal("stub adapter not registered")
	}
	hs, err := a.Scan()
	if err != nil {
		t.Fatalf("Scan on missing file: %v", err)
	}
	if len(hs) != 0 {
		t.Errorf("Scan on missing file returned %d hooks; want 0", len(hs))
	}
}

// TestStub_PreservesForeignContent: installing over a settings file
// that already carries foreign hooks and unrelated fields keeps both.
func TestStub_PreservesForeignContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "theme": "dark",
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "other-tool prime"}]}
    ]
  }
}`)
	if err := os.WriteFile(filepath.Join(dir, "stub-settings.json"), pre, 0o600); err != nil {
		t.Fatal(err)
	}

	a, ok := adapters.Lookup("stub")
	if !ok {
		t.Fatal("stub adapter not registered")
	}
	if err := a.Install(hooks.DefaultBase()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "stub-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `"theme"`) {
		t.Error("foreign top-level field dropped by install")
	}
	if !strings.Contains(got, "other-tool prime") {
		t.Error("foreign hook dropped by install")
	}

	scanned, err := a.Scan()
	if err != nil {
		t.Fatal(err)
	}
	var foreign, guild int
	for _, h := range scanned {
		if h.GuildOwned {
			guild++
		} else {
			foreign++
		}
	}
	if foreign != 1 || guild != 3 {
		t.Errorf("scan after install: %d foreign / %d guild; want 1 / 3", foreign, guild)
	}
}
