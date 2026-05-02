package mcp

import (
	"context"
	"log/slog"
	"strconv"
	"syscall"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/hints"
)

// currentHintsEngine holds the engine bound to the most recently
// Register()-ed MCP server. Fresh value per Register call so test
// isolation (each test rebuilds a server against its own $HOME) works
// transparently — the old engine is garbage-collected once the test
// closes its server handle.
//
// The engine is process-scoped in production (one server per process)
// so this pointer behaves like a singleton for live use.
var currentHintsEngine *hints.Engine

// initHintsEngine builds a fresh hints.Engine bound to the current
// quest.db (whichever qdbPath resolves to at call time) and replaces
// the package-level currentHintsEngine pointer. Safe to call repeatedly.
//
// Returns nil on open/load failure so callers can wire a nil
// EvaluateHints and let tool handlers proceed without hints rather than
// failing startup.
//
//nolint:contextcheck // called during Register (no per-request ctx); writes use per-Evaluate ctx later
func initHintsEngine() *hints.Engine {
	ctx := context.Background()
	closeCurrentHintsEngine()

	db, err := openQuestDB(ctx)
	if err != nil {
		slog.Debug("mcp: hints: cannot open quest.db; hints disabled", "err", err)
		currentHintsEngine = nil
		return nil
	}

	sessionID := strconv.Itoa(syscall.Getpid())
	eng := hints.NewEngine(hints.NewStore(db), sessionID, hints.EraMCP)
	eng.Logger = slog.Default()
	if err := eng.LoadRules(ctx); err != nil {
		slog.Debug("mcp: hints: load rules failed; hints disabled", "err", err)
		_ = db.Close()
		currentHintsEngine = nil
		return nil
	}
	currentHintsEngine = eng
	return eng
}

func closeCurrentHintsEngine() {
	if currentHintsEngine == nil {
		return
	}
	if err := currentHintsEngine.Close(); err != nil {
		slog.Debug("mcp: hints: close old engine failed", "err", err)
	}
	currentHintsEngine = nil
}

// hintsBridge returns a command.EvaluateHintsFunc wired to the current
// engine, or nil when the engine couldn't be built.
//
// Called multiple times per Register (once per Deps builder — lore-side
// and quest-side). All calls within a single Register share the same
// engine so one Context observes every tool's CallEvents; without
// sharing, hintsRecordBootstrap updates the last-built engine while the
// earlier-built engine's bridge closure still routes earlier-bound tools
// to the stale engine, causing no-session-start to fire after
// guild_session_start was called (QUEST-72). Register() resets
// currentHintsEngine to nil at entry so tests still get a fresh engine
// per server rebuild.
func hintsBridge() command.EvaluateHintsFunc {
	eng := currentHintsEngine
	if eng == nil {
		eng = initHintsEngine()
	}
	if eng == nil {
		return nil
	}
	return hints.Bridge(eng)
}

// hintsRecordBootstrap pushes a synthetic CallEvent into the current
// engine's Context so the bootstrap tools (guild_session_start,
// quest_bounties) — which bypass the command wrapper — don't break
// rules like no-session-start that rely on observing them.
func hintsRecordBootstrap(toolName string) {
	eng := currentHintsEngine
	if eng == nil {
		return
	}
	eng.Context().RecordEvent(hints.CallEvent{
		Tool: toolName,
	})
}
