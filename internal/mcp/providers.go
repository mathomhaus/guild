package mcp

import (
	"sync"

	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/hints"
)

// Providers bundles the lazily-resolved state one server instance pulls
// from at tool-call time: the lore-side embed provider, the quest-side
// embed provider, the hints engine, and the auto-backfill once-guard.
//
// Two ownership modes:
//
//   - Per-server (default): every default construction (Serve, build,
//     Register) creates a fresh bundle, preserving the historical
//     one-bundle-per-server-rebuild lifecycle of the stdio server.
//   - Shared (multi-session host): the host calls NewProviders once and
//     passes the same bundle to every per-connection NewServer call, so
//     all sessions share one embedder, one hints engine, and one
//     backfill trigger instead of N.
//
// Building a server against a shared bundle never resets or closes the
// bundle's state; lifecycle stays with whoever constructed it.
type Providers struct {
	// embed is the lore-side embed lazy-resolver. Every lore tool
	// handler pulls *lore.EmbedDeps from it via embedFromDeps; a nil
	// *EmbedDeps return (meta.embedder_state != "enabled") branches to
	// BM25+stopwords exactly like Phase 0. QUEST-219.
	embed *embedProvider

	// questEmbed is the quest-side embed lazy-resolver. quest_search
	// pulls *quest.QuestEmbedDeps from it via questEmbedFromDeps; a nil
	// return means BM25-only. Shares the lore-side Embedder; builds its
	// own QuestCorpus Index against quest.db. QUEST-258.
	questEmbed *questEmbedProvider

	// hintsMu guards hintsEngine: with a shared bundle, concurrent
	// per-connection registrations may race through hintsBridge.
	hintsMu sync.Mutex
	// hintsEngine is built lazily on the first hintsBridge call and
	// shared by every Deps builder against this bundle. See hints.go.
	hintsEngine *hints.Engine

	// backfill is the auto-backfill once-guard. One per bundle so a
	// shared bundle fires the trigger once per host process, not once
	// per connection. See embed_autobackfill.go.
	backfill *backfillGate
}

// NewProviders constructs a fresh provider bundle wired to the default
// ~/.guild DB locations. Multi-session hosts call this once and pass
// the bundle to every NewServer; the default stdio path constructs one
// implicitly per server build.
func NewProviders() *Providers {
	p := &Providers{backfill: &backfillGate{}}
	p.embed = newEmbedProvider(openLoreDB, newLogger())
	p.embed.backfill = p.backfill
	// Embedder backend selection (ADR-006 Phase 4). Read [embed].backend
	// once at bundle construction and stamp it onto the lazy resolver. The
	// default (local-bge) is the byte-identical local path; a configured
	// alternate backend engages inside WireEmbedDeps. A config-load error is
	// swallowed to the default backend: the embedder seam must never fail
	// server boot, exactly like the rest of this bundle's lazy wiring.
	if cfg, err := config.Load(nil); err == nil && cfg != nil {
		p.embed.backend = cfg.Embed.Backend
		p.embed.model = cfg.Embed.Model
	}
	p.questEmbed = newQuestEmbedProvider(p.embed, openQuestDB, newLogger())
	return p
}

// processProviders tracks the most recent bundle constructed by the
// default path so the next default construction can close its hints
// engine, releasing the prior engine's quest.db handle. This preserves
// the pre-bundle lifecycle where each server rebuild closed the
// previous engine (one server per process in production; many rebuilds
// per process in tests). Externally supplied bundles are never tracked
// here and never closed by another build.
var processProviders *Providers

// newProcessProviders builds the default per-server bundle, closing the
// previous default bundle's hints engine first. Not safe for concurrent
// use; concurrent server construction must pass an explicit bundle via
// Options.Providers.
func newProcessProviders() *Providers {
	if processProviders != nil {
		processProviders.closeHintsEngine()
	}
	p := NewProviders()
	processProviders = p
	return p
}
