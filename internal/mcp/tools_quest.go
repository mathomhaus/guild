package mcp

import (
	"context"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// quest_post generated from internal/quest.PostCommand via the command
// registry. See register.go for the call site.

// quest_accept is now generated from internal/quest.AcceptCommand via
// the command registry (QUEST-44). See register.go for the call site
// and docs/architecture/COMMAND_REGISTRY.md for the pattern.

// quest_journal is generated from internal/quest.JournalCommand via
// the command registry. See register.go for the call site.

// quest_clear is generated from internal/quest.ClearCommand via the
// command registry. See register.go for the call site.

// quest_brief is generated from internal/quest.BriefCommand via the
// command registry. See register.go for the call site.

// ---------------------------------------------------------------------------
// quest_bounties
// ---------------------------------------------------------------------------

type questBountiesInput struct {
	BriefOnly bool   `json:"brief_only,omitempty" jsonschema:"when true, return only the last briefing"`
	Project   string `json:"project,omitempty"`
}

var questBountiesTool = &sdkmcp.Tool{
	Name:        "quest_bounties",
	Description: "On-demand session snapshot: brief, oath, echoes, top task, and parallel candidates.",
}

func (c *serverCore) registerQuestBounties(s *sdkmcp.Server) {
	sdkmcp.AddTool(s, questBountiesTool, c.handleQuestBounties)
}

func (c *serverCore) handleQuestBounties(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	in questBountiesInput,
) (*sdkmcp.CallToolResult, any, error) {
	start := time.Now()
	var handlerErr error
	var respBytes uint
	//nolint:gocritic // ptrToRefParam — defer reads late-bound values
	defer c.recordMCPTelemetry(ctx, "quest_bounties", start, &handlerErr, &respBytes)
	c.providers.recordHintsBootstrap("quest_bounties")

	pid, err := c.resolveProject(ctx, in.Project)
	if err != nil {
		handlerErr = err
		return toolErrorf("%s", err), nil, nil
	}
	body, _ := renderBounties(ctx, pid, in.BriefOnly)
	if body == "" {
		body = emptyBountiesSkeleton()
	}
	respBytes = uint(len(body))
	return textResult(body), nil, nil
}

// quest_list + quest_pulse generated from internal/quest registry
// specs. See register.go for call sites.

// quest_epic (campaign bulk-set) generated from internal/quest.EpicCommand via
// the command registry. Accepts campaign= (primary) or epic= (alias) as the
// campaign name field. See register.go for the call site.

// quest_active is generated from internal/quest.ActiveCommand via the
// command registry. See register.go for the call site.

// quest_forfeit is generated from internal/quest.ForfeitCommand via
// the command registry. See register.go for the call site.
