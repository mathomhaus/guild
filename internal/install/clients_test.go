package install

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClient_Detected_CLIOnPath verifies that Detected() returns true when
// the CLIProbe binary is found on PATH.
func TestClient_Detected_CLIOnPath(t *testing.T) {
	// Use "go" as a probe — it's always available in the test environment.
	c := Client{
		Name:     "TestClient",
		CLIProbe: "go",
	}
	if !c.Detected() {
		t.Error("Detected() = false; expected true when CLI binary ('go') is on PATH")
	}
}

// TestClient_Detected_CLIAbsent verifies that Detected() falls back to
// ConfigProbe when CLIProbe is not on PATH, and returns false when
// the config file doesn't exist either.
func TestClient_Detected_CLIAbsentConfigAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	c := Client{
		Name:        "TestClient",
		CLIProbe:    "nonexistent-cli-binary-xyzzy",
		ConfigProbe: "~/.nonexistent-config-xyzzy.json",
	}
	if c.Detected() {
		t.Error("Detected() = true; expected false when CLI absent and config absent")
	}
}

// TestClient_Detected_ConfigFallback verifies that Detected() returns true
// when CLIProbe is absent but ConfigProbe file exists.
func TestClient_Detected_ConfigFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create the config file that ConfigProbe points to.
	cfgPath := filepath.Join(home, ".testclient.json")
	if err := os.WriteFile(cfgPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := Client{
		Name:        "TestClient",
		CLIProbe:    "nonexistent-cli-binary-xyzzy",
		ConfigProbe: "~/.testclient.json",
	}
	if !c.Detected() {
		t.Error("Detected() = false; expected true when config file exists")
	}
}

// TestClient_Detected_CLITakesPrecedence verifies that CLIProbe is checked
// before ConfigProbe — if CLI is found, result is true even with no config.
func TestClient_Detected_CLITakesPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No config file present, but CLI ("go") is on PATH.
	c := Client{
		Name:        "TestClient",
		CLIProbe:    "go",
		ConfigProbe: "~/.nonexistent-config-xyzzy.json",
	}
	if !c.Detected() {
		t.Error("Detected() = false; expected true when CLI is on PATH")
	}
}

// TestClient_Detected_NeitherProbe verifies that a Client with both probes
// empty (no CLI, no config) returns false.
func TestClient_Detected_NeitherProbe(t *testing.T) {
	c := Client{
		Name: "TestClient",
	}
	if c.Detected() {
		t.Error("Detected() = true; expected false when neither probe is set")
	}
}

// TestClients_WellFormed verifies that every entry in the Clients slice has
// a Name, an InstallArgv, and at least one probe set.
func TestClients_WellFormed(t *testing.T) {
	for _, c := range Clients {
		if c.Name == "" {
			t.Errorf("client with empty Name in Clients slice")
		}
		if c.InstallArgv == nil {
			t.Errorf("client %q has nil InstallArgv", c.Name)
		} else if argv := c.InstallArgv(filepath.Join(t.TempDir(), "guild")); len(argv) == 0 {
			t.Errorf("client %q InstallArgv returned empty slice", c.Name)
		}
		if c.CLIProbe == "" && c.ConfigProbe == "" {
			t.Errorf("client %q has neither CLIProbe nor ConfigProbe", c.Name)
		}
	}
}

// TestClients_SliceLength verifies that Clients contains exactly the
// documented clients (Claude Code, Cursor, Codex).
func TestClients_SliceLength(t *testing.T) {
	if len(Clients) != 3 {
		t.Errorf("Clients length = %d; expected 3", len(Clients))
	}
}

// TestClient_ConfigProbe_NestedPath verifies that ConfigProbe correctly
// resolves a nested path like "~/.cursor/mcp.json".
func TestClient_ConfigProbe_NestedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	nested := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(nested, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := Client{
		Name:        "Cursor",
		CLIProbe:    "nonexistent-cli-binary-xyzzy",
		ConfigProbe: "~/.cursor/mcp.json",
		InstallArgv: func(b string) []string {
			return []string{"cursor", "mcp", "add", "guild", "--", b, "mcp", "serve"}
		},
	}
	if !c.Detected() {
		t.Error("Detected() = false; expected true for nested config path")
	}
}
