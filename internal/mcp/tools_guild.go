package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/session"
)

// ---------------------------------------------------------------------------
// guild_set_project
// ---------------------------------------------------------------------------

// setProjectInput is the typed input for guild_set_project.
// Single required string — intentionally minimal.
type setProjectInput struct {
	Project string `json:"project" jsonschema:"directory basename to activate (e.g. 'guild') — REQUIRED"`
}

var setProjectTool = &sdkmcp.Tool{
	Name:        "guild_set_project",
	Description: "Switch the active project for the rest of this session. Use to work across projects in one MCP connection.",
}

func registerSetProject(s *sdkmcp.Server) {
	sdkmcp.AddTool(s, setProjectTool, handleSetProject)
}

func handleSetProject(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	in setProjectInput,
) (*sdkmcp.CallToolResult, any, error) {
	start := time.Now()
	var handlerErr error
	var respBytes uint
	//nolint:gocritic // ptrToRefParam — defer reads late-bound values
	defer recordMCPTelemetry(ctx, "guild_set_project", start, &handlerErr, &respBytes)

	name := strings.TrimSpace(in.Project)
	if name == "" {
		handlerErr = fmt.Errorf("empty project name")
		return toolErrorf("guild_set_project: empty project name — pass project='<name>'"), nil, nil
	}
	if err := session.SetActiveProject(ctx, name); err != nil {
		handlerErr = err
		return toolFatalf("persist active project %q: %v", name, err), nil, nil
	}
	body := fmt.Sprintf("🔀 active project set to %q", name)
	respBytes = uint(len(body))
	return textResult(body), nil, nil
}

// ---------------------------------------------------------------------------
// guild_status — mid-session reorientation (QUEST-31)
// ---------------------------------------------------------------------------

// guildStatusInput is intentionally minimal — guild_status is the
// mid-session read of the same snapshot session_start prints on
// bootstrap. BriefOnly narrows the output to just the last briefing
// for callers that only need the handoff context.
type guildStatusInput struct {
	BriefOnly bool   `json:"brief_only,omitempty" jsonschema:"when true, return only the last briefing"`
	Project   string `json:"project,omitempty" jsonschema:"project override; defaults to active session project"`
}

var guildStatusTool = &sdkmcp.Tool{
	Name:        "guild_status",
	Description: "On-demand dashboard mirroring guild_session_start — last brief, oath, echoes, top bounty, parallelism. Call anytime to re-orient mid-session without re-bootstrapping.",
}

func registerGuildStatus(s *sdkmcp.Server) {
	sdkmcp.AddTool(s, guildStatusTool, handleGuildStatus)
}

func handleGuildStatus(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	in guildStatusInput,
) (*sdkmcp.CallToolResult, any, error) {
	pid, err := resolveProject(ctx, in.Project)
	if err != nil {
		return toolErrorf("%s", err), nil, nil
	}
	body, _ := renderBounties(ctx, pid, in.BriefOnly)
	if body == "" {
		body = emptyBountiesSkeleton()
	}
	return textResult(body), nil, nil
}

// Archive/restore is intentionally CLI-only — it's a human-facing
// export action ("commit my snapshot to git"), not something agents
// need mid-session. Users run `guild quest archive` / `guild lore
// archive` from the shell. See docs/architecture/COMMAND_REGISTRY.md.
