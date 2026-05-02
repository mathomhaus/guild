package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Client describes how to register guild with one MCP client.
// All fields are data; no methods beyond Detected().
type Client struct {
	Name        string // human-readable (e.g. "Claude Code")
	CLIProbe    string // binary to test via exec.LookPath (e.g. "claude")
	ConfigProbe string // fallback: path to test if CLI absent (e.g. "~/.claude.json")

	// InstallArgv returns the structured argv for the install command given
	// the guild binary path. Using []string avoids strings.Fields re-parsing,
	// which breaks any binary path that contains spaces.
	InstallArgv func(binPath string) []string

	// ListArgv returns the argv for listing registered MCP servers. Used
	// to short-circuit install when guild is already registered — avoids
	// the noisy "MCP server guild already exists in user config" error
	// on repeat `guild init` runs. Optional: a nil ListArgv disables the
	// pre-check and preserves historical behaviour.
	ListArgv func() []string

	ManualSnippet string // optional: fallback JSON/TOML for clients without a CLI
}

// InstallCmdDisplay returns a copy-paste-safe shell representation of the
// install command. The binary path is quoted so paths with spaces round-trip
// correctly through a shell.
func (c Client) InstallCmdDisplay(binPath string) string {
	argv := c.InstallArgv(binPath)
	if len(argv) == 0 {
		return ""
	}
	// Quote only the binary-path token; the surrounding tokens are static CLI
	// words that never contain spaces, so they don't need quoting.
	out := make([]string, len(argv))
	copy(out, argv)
	for i, tok := range out {
		if tok == binPath {
			out[i] = fmt.Sprintf("%q", tok)
		}
	}
	return strings.Join(out, " ")
}

// Detected returns true if this client appears installed on the system.
// Check order: CLI on PATH → config file exists.
func (c Client) Detected() bool {
	if c.CLIProbe != "" {
		if _, err := exec.LookPath(c.CLIProbe); err == nil {
			return true
		}
	}
	if c.ConfigProbe != "" {
		home, err := os.UserHomeDir()
		if err == nil {
			path := filepath.Join(home, strings.TrimPrefix(c.ConfigProbe, "~/"))
			if _, err := os.Stat(path); err == nil {
				return true
			}
		}
	}
	return false
}

// Clients is the supported-client registry. Adding a new client means
// adding a struct literal here and (optionally) a unit test.
var Clients = []Client{
	{
		Name:        "Claude Code",
		CLIProbe:    "claude",
		ConfigProbe: "~/.claude.json",
		InstallArgv: func(b string) []string {
			return []string{"claude", "mcp", "add", "guild", "--scope", "user", "--", b, "mcp", "serve"}
		},
		ListArgv: func() []string { return []string{"claude", "mcp", "list"} },
	},
	{
		Name:        "Cursor",
		CLIProbe:    "cursor",
		ConfigProbe: "~/.cursor/mcp.json",
		InstallArgv: func(b string) []string {
			return []string{"cursor", "mcp", "add", "guild", "--", b, "mcp", "serve"}
		},
		ListArgv: func() []string { return []string{"cursor", "mcp", "list"} },
	},
	{
		Name:        "Codex (OpenAI)",
		CLIProbe:    "codex",
		ConfigProbe: "~/.codex/config.toml",
		InstallArgv: func(b string) []string {
			return []string{"codex", "mcp", "add", "guild", "--", b, "mcp", "serve"}
		},
		// ListArgv left nil until `codex mcp list` output shape is verified
		// against a real run; nil disables the pre-check and the install
		// attempt proceeds as before.
	},
}
