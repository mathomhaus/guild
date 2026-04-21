package quest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/quest"
)

// TestFulfillCommand_CobraSurface verifies the primary verb + backward-compat
// alias both land on cobra: `quest fulfill` is the primary subcommand, and
// `quest clear` is surfaced via CLIAliases.
func TestFulfillCommand_CobraSurface(t *testing.T) {
	parent := &cobra.Command{Use: "quest"}
	parent.PersistentFlags().StringP("project", "p", "", "project")
	quest.FulfillCommand.BindCobra(parent, fakeDeps(t))

	sub := findSubcommand(parent, "fulfill")
	if sub == nil {
		t.Fatal("fulfill subcommand not registered")
	}
	if got, want := sub.Use, "fulfill QUEST_ID"; got != want {
		t.Errorf("Use=%q want %q", got, want)
	}
	// cobra Aliases should include "clear" for muscle-memory users.
	foundAlias := false
	for _, a := range sub.Aliases {
		if a == "clear" {
			foundAlias = true
			break
		}
	}
	if !foundAlias {
		t.Errorf("expected `clear` among aliases, got %v", sub.Aliases)
	}
	for _, want := range []string{"report", "json"} {
		if sub.Flags().Lookup(want) == nil {
			t.Errorf("--%s flag missing", want)
		}
	}
	// project inherited, not re-registered
	if sub.LocalFlags().Lookup("project") != nil {
		t.Error("--project re-registered locally; should inherit from parent")
	}
}

// TestFulfillCommand_MCPSurface verifies the canonical MCP tool is named
// `quest_fulfill` and carries the new "Fulfill a quest" description.
func TestFulfillCommand_MCPSurface(t *testing.T) {
	tool := quest.FulfillCommand.BuildMCPForTest(fakeDeps(t))
	if tool.Name != "quest_fulfill" {
		t.Errorf("Name=%q want quest_fulfill", tool.Name)
	}
	if !strings.HasPrefix(tool.Description, "Fulfill a quest") {
		t.Errorf("Description=%q", tool.Description)
	}
	buf, _ := json.Marshal(tool.InputSchema)
	schema := string(buf)
	for _, want := range []string{`"quest_id"`, `"report"`, `"project"`} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema missing %s:\n%s", want, schema)
		}
	}
}

// TestClearCommand_MCPBackwardCompat verifies the MCP-only alias tool is
// still advertised under the legacy name `quest_clear` so agents trained
// on the pre-QUEST-106 verb continue to work, and that its rendered output
// includes the deprecation notice. QUEST-138 / LORE-122.
func TestClearCommand_MCPBackwardCompat(t *testing.T) {
	tool := quest.ClearCommand.BuildMCPForTest(fakeDeps(t))
	if tool.Name != "quest_clear" {
		t.Errorf("Name=%q want quest_clear (backward-compat alias)", tool.Name)
	}
	// Description should still reference fulfill semantics.
	if !strings.Contains(tool.Description, "Fulfill") {
		t.Errorf("alias description should reference fulfill; got %q", tool.Description)
	}

	// The MCPFormat for the alias must include the deprecation notice so
	// agents get a migration gradient. Build a minimal FulfillOutput and
	// render it through the alias formatter.
	out := quest.FulfillOutput{
		Result: &quest.FulfillResult{
			Cleared: &quest.Quest{ID: "QUEST-99", Subject: "test"},
		},
	}
	rendered := quest.ClearCommand.MCPFormat(command.MCPSink{}, out)
	if !strings.Contains(rendered, "deprecated") {
		t.Errorf("ClearCommand MCP output missing deprecation notice; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "quest_fulfill") {
		t.Errorf("ClearCommand MCP output missing 'quest_fulfill' pointer; got:\n%s", rendered)
	}
	// The deprecation notice must appear AFTER the success line.
	successIdx := strings.Index(rendered, "QUEST-99")
	deprIdx := strings.Index(rendered, "deprecated")
	if successIdx < 0 || deprIdx < 0 || deprIdx <= successIdx {
		t.Errorf("deprecation notice not positioned after success line; rendered:\n%s", rendered)
	}

	// quest_fulfill's MCPFormat must NOT include the deprecation notice.
	renderedFulfill := quest.FulfillCommand.MCPFormat(command.MCPSink{}, out)
	if strings.Contains(renderedFulfill, "deprecated") {
		t.Errorf("FulfillCommand MCP output must not include deprecation notice; got:\n%s", renderedFulfill)
	}
}

// findSubcommand returns the direct child of parent named name, or nil.
// Shared across conformance tests in this package.
func findSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
