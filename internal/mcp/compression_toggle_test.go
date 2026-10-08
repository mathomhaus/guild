package mcp

import (
	"context"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCompression_AbsentByDefault locks the ADR-006 Phase 7 disabled-absence
// guarantee at the kernel/MCP surface: with the default config the
// compression module's tools must NOT appear in tools/list. The existing
// TestTools_RegisteredCount already pins the exact default set; this is the
// targeted assertion for the two compression verbs.
func TestCompression_AbsentByDefault(t *testing.T) {
	isolateHome(t)
	names := listToolNames(t)
	for _, n := range names {
		if n == "compression_compress" || n == "compression_retrieve" {
			t.Fatalf("compression tool %q present on the DEFAULT surface (must be absent)", n)
		}
	}
}

// TestCompression_PresentWhenEnabled proves the toggle wires the module's
// tools onto the MCP surface when [modules].compression is turned on via the
// GUILD_MODULE_COMPRESSION env override.
func TestCompression_PresentWhenEnabled(t *testing.T) {
	isolateHome(t)
	t.Setenv("GUILD_MODULE_COMPRESSION", "1")

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	res, err := client.ListTools(context.Background(), &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	have := map[string]bool{}
	for _, tool := range res.Tools {
		have[tool.Name] = true
	}
	for _, want := range []string{"compression_compress", "compression_retrieve"} {
		if !have[want] {
			t.Errorf("enabled compression module missing tool %q from tools/list", want)
		}
	}
}
