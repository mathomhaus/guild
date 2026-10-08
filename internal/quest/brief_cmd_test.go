package quest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/quest"
)

func TestBriefCommand_CobraSurface(t *testing.T) {
	parent := &cobra.Command{Use: "quest"}
	parent.PersistentFlags().StringP("project", "p", "", "project")
	quest.BriefCommand.BindCobra(parent, fakeDeps(t))

	sub := findSubcommand(parent, "brief")
	if sub == nil {
		t.Fatal("brief subcommand not registered")
	}
	if got, want := sub.Use, "brief TEXT..."; got != want {
		t.Errorf("Use=%q want %q", got, want)
	}
}

func TestBriefCommand_MCPSurface(t *testing.T) {
	tool := quest.BriefCommand.BuildMCPForTest(fakeDeps(t))
	if tool.Name != "quest_brief" {
		t.Errorf("Name=%q want quest_brief", tool.Name)
	}
	buf, _ := json.Marshal(tool.InputSchema)
	schema := string(buf)
	for _, want := range []string{`"text"`, `"project"`} {
		if !strings.Contains(schema, want) {
			t.Errorf("MCP schema missing %s", want)
		}
	}
	// The hook-mode switches are CLI-only: they must never surface on
	// the MCP tool schema.
	for _, banned := range []string{`"auto"`, `"capture"`} {
		if strings.Contains(schema, banned) {
			t.Errorf("MCP schema must not expose CLI-only arg %s", banned)
		}
	}
}
