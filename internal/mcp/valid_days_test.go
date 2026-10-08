package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoreValidDaysFromConfig verifies the MCP-surface provider behind
// command.Deps.LoreValidDays: it reads [inscribe.valid_days] from the
// user-wide config per call (so a long-lived server observes edits) and
// falls back to nil, meaning built-in kind defaults, when the config
// fails to load.
func TestLoreValidDaysFromConfig(t *testing.T) {
	home := isolateHome(t)
	guildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(guildDir, 0o700); err != nil {
		t.Fatalf("mkdir .guild: %v", err)
	}
	cfgPath := filepath.Join(guildDir, "config.toml")

	// No config file: built-in defaults from the config package.
	got := loreValidDaysFromConfig()
	if got["research"] != 30 || got["decision"] != 180 {
		t.Errorf("zero-config windows: got %v, want research=30 decision=180", got)
	}

	// Config written mid-session: the next call observes it.
	if err := os.WriteFile(cfgPath,
		[]byte("[inscribe.valid_days]\nresearch = 1\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	got = loreValidDaysFromConfig()
	if got["research"] != 1 {
		t.Errorf("configured research window: got %d, want 1", got["research"])
	}
	if got["decision"] != 180 {
		t.Errorf("decision should keep default 180, got %d", got["decision"])
	}

	// Invalid config (unknown kind): load fails, provider degrades to
	// nil so writes fall back to built-in kind defaults instead of
	// erroring.
	if err := os.WriteFile(cfgPath,
		[]byte("[inscribe.valid_days]\nreasearch = 7\n"), 0o600); err != nil {
		t.Fatalf("rewrite config.toml: %v", err)
	}
	if got = loreValidDaysFromConfig(); got != nil {
		t.Errorf("broken config should yield nil (built-in defaults), got %v", got)
	}
}
