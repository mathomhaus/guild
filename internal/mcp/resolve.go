package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

// guildProjectEnv is the env var checked as the last-ditch fallback in
// the MCP project-resolution order. Matches the CLI's GUILD_PROJECT
// convention so a single export works for both.
const guildProjectEnv = "GUILD_PROJECT"

// resolveProject is the shared entry point every non-bootstrap tool
// handler uses to figure out which project it operates on. Threads ctx
// through the core's SessionStore so a canceled tool call propagates
// into the file IO, and so each server instance resolves against its
// own session identity rather than a process-global one.
//
// Returns the resolved project id and nil on success, or ("", err) with
// a pre-formatted [error] message ready to become the tool's isError
// payload. Callers typically:
//
//	pid, err := c.resolveProject(ctx, in.Project)
//	if err != nil {
//	    return toolErrorf("%s", err), nil, nil
//	}
//
// "explicit arg" covers the `project` field many tools expose for cases
// where the agent wants to override the active project without a
// separate guild_set_project call.
func (c *serverCore) resolveProject(ctx context.Context, argProject string) (string, error) {
	return c.sessions.ResolveForMCP(ctx, strings.TrimSpace(argProject), os.Getenv(guildProjectEnv))
}

// resolveProjectAutoBootstrap wraps resolveProject with implicit auto-bootstrap:
// when no active project is set AND the argProject is empty, it attempts to
// infer the project from cwd via inferProjectFromCWD. On a clean resolution,
// it persists the project through the core's SessionStore silently and writes
// a narration line into the narration pointer stored in ctx (placed there by
// the MCP handler wrapper via WithNarrationPtr). The tool then proceeds
// normally.
//
// Error-shape matrix (per ENTRY-25):
//  1. cwd not in a git repo          → unchanged "call guild_session_start" error
//  2. cwd in git but path not registered → unchanged "not registered, run guild init" error
//  3. cwd resolves unambiguously     → NEW: auto-bootstrap transparently + narration
//  4. cwd matches multiple projects  → behaves as case 3 (first match wins); case 4
//     is deferred because LookupByPath returns the first match and the schema has
//     UNIQUE path constraint, making true duplicates impossible at the DB level.
//
// Telemetry: auto-bootstrap firings are logged at slog.Debug level with
// project id so we can measure reconnect frequency. Usage.log wiring is
// deferred (QUEST-54 extension) — slog.Debug is sufficient for now.
func (c *serverCore) resolveProjectAutoBootstrap(ctx context.Context, argProject string) (string, error) {
	pid, err := c.sessions.ResolveForMCP(ctx, strings.TrimSpace(argProject), os.Getenv(guildProjectEnv))
	if err == nil {
		// Fast path: project already set or explicit arg given.
		return pid, nil
	}

	// Only attempt auto-bootstrap when no explicit arg was given AND the
	// error looks like "no active project set" (the exact text from
	// session.ResolveForMCP's step-4 error). If the explicit arg was set
	// but invalid, or another error occurred, surface it unchanged.
	if strings.TrimSpace(argProject) != "" {
		return "", err
	}
	if !strings.Contains(err.Error(), "no active project set") {
		return "", err
	}

	// Attempt cwd-based inference. Errors propagate verbatim (they already
	// contain recovery guidance from project.Resolve):
	//   - ErrNotInGitRepo → "not inside a git repository"
	//   - ErrNotRegistered → "project X not registered — run 'guild init'"
	//
	// inferProject returns (projectID, viaWorktreeFallback, err) per
	// QUEST-67, anchored on this server's connection cwd (process cwd on
	// the stdio path, shim preamble cwd on the daemon path). The fallback
	// flag signals the resolver used the worktree's main-repo path; we
	// surface it in the narration so the agent+user see how the project
	// was reached.
	inferred, viaWorktreeFallback, inferErr := c.inferProject(ctx)
	if inferErr != nil {
		// Auto-inference failed; return the original "no active project" error
		// so the agent gets the standard bootstrap guidance, not a confusing
		// inference-failure message on an otherwise-normal tool call.
		return "", err
	}

	// Persist the inferred project for this session identity so subsequent
	// tool calls in the same session don't need to re-infer.
	if setErr := c.sessions.SetActiveProject(ctx, inferred); setErr != nil {
		// Persist failure is rare (unwritable home dir). Fall through to
		// success anyway — the tool should still work this call even if
		// we couldn't cache the result.
		slog.Debug("auto-bootstrap: failed to persist active project",
			"project", inferred, "err", setErr)
	}

	// Emit slog.Debug for telemetry/observability. Usage.log wiring
	// deferred to QUEST-54 extension — slog.Debug is sufficient per spec.
	slog.Debug("auto-bootstrap: resolved project from cwd", "project", inferred)

	// Write narration line into the context-carried pointer so the MCP
	// handler wrapper can prepend it to the tool's response body.
	if ptr := command.MCPNarrationPtrFromCtx(ctx); ptr != nil {
		if viaWorktreeFallback {
			*ptr = fmt.Sprintf("[auto-bootstrapped to project %q — inferred from worktree's main-repo path]", inferred)
		} else {
			*ptr = fmt.Sprintf("[auto-bootstrapped to project %q from cwd]", inferred)
		}
	}

	return inferred, nil
}
