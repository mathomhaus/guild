package mcp

import (
	"context"
	"sort"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// eval_module_test.go is the MCP-surface half of the ADR-006 Phase 6
// enabled/disabled acceptance: the eval module ships OFF by default, so its
// eval_run tool must be ABSENT from tools/list with a silent config and
// PRESENT only when an operator enables it (here via GUILD_MODULE_EVAL=1).
// This is the same structural toggle proof TestE2EModuleToggle runs for lore,
// scoped to a unit-level in-memory server.

// toolNamesOn lists the server's tools/list names for the current env.
func toolNamesOn(t *testing.T) []string {
	t.Helper()
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
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// TestEvalModule_AbsentByDefault asserts eval_run does not appear on the MCP
// surface with a silent config (the byte-identical-default guarantee), while
// the core tools remain present.
func TestEvalModule_AbsentByDefault(t *testing.T) {
	isolateHome(t)
	names := toolNamesOn(t)
	for _, n := range names {
		if n == "eval_run" {
			t.Fatal("eval_run must be absent from tools/list by default (eval is off)")
		}
	}
	// Core tools are unaffected.
	if !contains(names, "quest_post") || !contains(names, "lore_appraise") {
		t.Errorf("core tools missing from default surface: %v", names)
	}
}

// TestEvalModule_PresentWhenEnabled asserts that turning the module on via the
// GUILD_MODULE_EVAL env override surfaces eval_run, with a non-empty, lean
// description, and that core tools still register alongside it.
func TestEvalModule_PresentWhenEnabled(t *testing.T) {
	isolateHome(t)
	t.Setenv("GUILD_MODULE_EVAL", "1")

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
	var found *sdkmcp.Tool
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Name == "eval_run" {
			found = tool
		}
	}
	if found == nil {
		t.Fatalf("eval_run absent from tools/list with GUILD_MODULE_EVAL=1; got %v", names)
	}
	if found.Description == "" {
		t.Error("eval_run registered without a description")
	}
	if len(found.Description) > 600 {
		t.Errorf("eval_run description too long: %d chars", len(found.Description))
	}
	// Enabling eval must not strip the core surface.
	if !contains(names, "quest_post") || !contains(names, "lore_appraise") {
		t.Errorf("core tools missing when eval enabled: %v", names)
	}
}

// TestEvalModule_RunInvokes drives eval_run end to end over the in-memory MCP
// transport, proving the wired tool actually executes the grid and returns a
// rendered verdict body (not an error).
func TestEvalModule_RunInvokes(t *testing.T) {
	isolateHome(t)
	t.Setenv("GUILD_MODULE_EVAL", "1")

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	result, err := client.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "eval_run",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool eval_run: %v", err)
	}
	if result.IsError {
		t.Fatalf("eval_run returned IsError=true: %s", textOf(result.Content))
	}
	body := textOf(result.Content)
	if !strings.Contains(body, "eval grid") {
		t.Errorf("eval_run body missing grid summary; got: %s", body)
	}
	if !strings.Contains(body, "parity") {
		t.Errorf("eval_run body missing parity line; got: %s", body)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
