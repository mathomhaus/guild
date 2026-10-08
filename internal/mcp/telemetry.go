package mcp

import (
	"context"
	"os"
	"time"

	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/telemetry"
)

// recordMCPTelemetry is the MCP-side equivalent of the CLI's
// recordTelemetry. It is wired into command.Deps.RecordTelemetry by
// buildMCPCommandDeps and buildMCPLoreDeps so the MCP adapter in
// internal/command/mcp.go can emit one usage.log row per CallTool.
//
// Resolution order for the project id:
//  1. the core's session identity (guild_session_start sets it)
//  2. GUILD_PROJECT env var
//  3. empty string (best-effort; telemetry.Record still writes the row)
//
// respBytesPtr points to the rendered response body byte count set by
// the handler wrapper after composing the final body. 0 on error paths.
//
// Errors from config.Load and telemetry.Record are swallowed: telemetry
// is best-effort and must never interrupt a tool call.
//
//nolint:gocritic // ptrToRefParam — errPtr/respBytesPtr observe late-bound values from defer
func (c *serverCore) recordMCPTelemetry(ctx context.Context, toolName string, start time.Time, errPtr *error, respBytesPtr *uint) {
	cfg, err := config.Load(nil)
	if err != nil {
		return // config unavailable — skip silently
	}

	pid, _ := c.sessions.ResolveForMCP(ctx, "", os.Getenv(guildProjectEnv))

	exit := 0
	if errPtr != nil && *errPtr != nil {
		exit = 1
	}

	var respBytes uint
	if respBytesPtr != nil {
		respBytes = *respBytesPtr
	}

	_ = telemetry.Record(ctx, cfg, pid, toolName, exit, time.Since(start), respBytes)
}

// recordMCPMiss is the MCP-side equivalent of the CLI's RecordMiss call
// inside lore appraise. Wired into command.Deps.RecordMiss by
// buildMCPLoreDeps so the lore_appraise handler can emit a misses.log
// row on zero results without importing internal/telemetry directly.
//
// Errors are swallowed (best-effort), matching CLI behaviour.
func recordMCPMiss(ctx context.Context, project, query string) {
	cfg, err := config.Load(nil)
	if err != nil {
		return
	}
	_ = telemetry.RecordMiss(ctx, cfg, project, query)
}
