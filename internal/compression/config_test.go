package compression

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/config"
)

// TestConfigMergerCapturesSection proves the Phase-3 RegisterModuleConfig seam
// wires the [compression] TOML section into the package snapshot, without any
// edit to internal/config's core struct. It writes a user-level config with a
// [compression] table, loads config, and checks CurrentSettings reflects it.
func TestConfigMergerCapturesSection(t *testing.T) {
	resetSettingsForTest()
	t.Cleanup(resetSettingsForTest)

	home := t.TempDir()
	t.Setenv("HOME", home)
	guildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(guildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `
[modules]
compression = true

[compression]
strategies = ["json", "diff"]
ccr_ttl = "90s"
dossier_compact = true
`
	if err := os.WriteFile(filepath.Join(guildDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Run Load from a cwd with no repo-level config so only the user layer
	// (and our [compression] section) applies.
	if _, err := config.Load(nil); err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	got := CurrentSettings()
	if len(got.Strategies) != 2 || got.Strategies[0] != "json" || got.Strategies[1] != "diff" {
		t.Errorf("strategies = %v, want [json diff]", got.Strategies)
	}
	if got.CCRTTL != 90*time.Second {
		t.Errorf("ccr_ttl = %v, want 90s", got.CCRTTL)
	}
	if !got.DossierCompact {
		t.Error("dossier_compact should be true")
	}
}

// TestConfigSilentLeavesDefaults proves a config with no [compression] section
// leaves the snapshot at its zero (off-equivalent) value, so the default path
// engages nothing.
func TestConfigSilentLeavesDefaults(t *testing.T) {
	resetSettingsForTest()
	t.Cleanup(resetSettingsForTest)

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".guild"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(nil); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got := CurrentSettings()
	if got.DossierCompact || got.CCRTTL != 0 || len(got.Strategies) != 0 {
		t.Errorf("silent config should leave zero settings, got %+v", got)
	}
}
