package mcp

import (
	"context"
	"log/slog"
	"strconv"
	"syscall"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/hints"
)

// The hints engine is bundle-scoped: one engine per Providers bundle so
// all Deps builders within a single server registration share it, and a
// multi-session host sharing one bundle shares one engine. The default
// path constructs a fresh bundle per server build, so test isolation
// (each test rebuilds a server against its own $HOME) works
// transparently, matching the pre-bundle per-rebuild lifecycle.

// buildHintsEngine builds a fresh hints.Engine bound to the current
// quest.db (whichever qdbPath resolves to at call time).
//
// Returns nil on open/load failure so callers can wire a nil
// EvaluateHints and let tool handlers proceed without hints rather than
// failing startup.
//
//nolint:contextcheck // called during registration (no per-request ctx); writes use per-Evaluate ctx later
func buildHintsEngine() *hints.Engine {
	ctx := context.Background()

	db, err := openQuestDB(ctx)
	if err != nil {
		slog.Debug("mcp: hints: cannot open quest.db; hints disabled", "err", err)
		return nil
	}

	sessionID := strconv.Itoa(syscall.Getpid())
	eng := hints.NewEngine(hints.NewStore(db), sessionID, hints.EraMCP)
	eng.Logger = slog.Default()
	if err := eng.LoadRules(ctx); err != nil {
		slog.Debug("mcp: hints: load rules failed; hints disabled", "err", err)
		_ = db.Close()
		return nil
	}
	return eng
}

// ensureHintsEngine returns the bundle's engine, building it on first
// use. A nil return means the engine could not be built (quest.db
// unreachable); the next call retries, matching the historical
// retry-on-nil behavior of hintsBridge.
func (p *Providers) ensureHintsEngine() *hints.Engine {
	p.hintsMu.Lock()
	defer p.hintsMu.Unlock()
	if p.hintsEngine == nil {
		p.hintsEngine = buildHintsEngine()
	}
	return p.hintsEngine
}

// closeHintsEngine closes and clears the bundle's engine. Called by the
// default construction path when a fresh process-default bundle
// replaces the previous one, so each rebuild releases the prior
// engine's quest.db handle exactly as before the bundle refactor.
// External bundle owners may call it on teardown.
func (p *Providers) closeHintsEngine() {
	p.hintsMu.Lock()
	defer p.hintsMu.Unlock()
	if p.hintsEngine == nil {
		return
	}
	if err := p.hintsEngine.Close(); err != nil {
		slog.Debug("mcp: hints: close old engine failed", "err", err)
	}
	p.hintsEngine = nil
}

// hintsBridge returns a command.EvaluateHintsFunc wired to the bundle's
// engine, or nil when the engine couldn't be built.
//
// Called multiple times per registration (once per Deps builder, the
// lore-side and the quest-side). All calls against one bundle share the
// same engine so one Context observes every tool's CallEvents; without
// sharing, recordHintsBootstrap updates the last-built engine while the
// earlier-built engine's bridge closure still routes earlier-bound tools
// to the stale engine, causing no-session-start to fire after
// guild_session_start was called (QUEST-72).
func (p *Providers) hintsBridge() command.EvaluateHintsFunc {
	eng := p.ensureHintsEngine()
	if eng == nil {
		return nil
	}
	return hints.Bridge(eng)
}

// recordHintsBootstrap pushes a synthetic CallEvent into the bundle's
// engine Context so the bootstrap tools (guild_session_start,
// quest_bounties), which bypass the command wrapper, don't break rules
// like no-session-start that rely on observing them. Read-only on the
// engine slot: a bundle whose engine was never built records nothing.
func (p *Providers) recordHintsBootstrap(toolName string) {
	p.hintsMu.Lock()
	eng := p.hintsEngine
	p.hintsMu.Unlock()
	if eng == nil {
		return
	}
	eng.Context().RecordEvent(hints.CallEvent{
		Tool: toolName,
	})
}
