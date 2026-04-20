package mcp

import (
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
)

// Register wires every guild tool onto s. Called exactly once from
// build() in server.go. bootstrap → always-on; no deferred tier.
func Register(s *sdkmcp.Server) {
	// Reset so hintsBridge builds one engine per server rebuild and all
	// Deps builders in this Register share it. See hints.go comment.
	currentHintsEngine = nil
	registerBootstrap(s)
	registerAlwaysOn(s)
}

// buildMCPCommandDeps constructs the quest-side Deps bundle. The
// registry's OpenDB opens quest.db; ResolveProj uses the auto-bootstrap
// resolver so MCP reconnects are invisible (QUEST-65). RecordTelemetry
// wires the MCP usage.log emitter so every tool call produces a row.
// PrependNarration enables the auto-bootstrap narration path in the MCP
// handler wrapper — when auto-bootstrap fires, the narration line is
// prepended to the tool's output body. OpenLoreDB is wired so
// quest_post's spec= param (QUEST-63) can atomically inscribe a
// kind=decision lore entry alongside the quest.
func buildMCPCommandDeps() command.Deps {
	return command.Deps{
		OpenDB:           openQuestDB,
		ResolveProj:      resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  recordMCPTelemetry,
		PrependNarration: true,
		OpenLoreDB:       openLoreDB,
		EvaluateHints:    hintsBridge(),
	}
}

// buildMCPLoreDeps is the lore-side sibling. Identical ResolveProj with
// auto-bootstrap (QUEST-65), but OpenDB opens lore.db. RecordMiss wires
// the misses.log emitter for lore_appraise zero-result queries.
func buildMCPLoreDeps() command.Deps {
	return command.Deps{
		OpenDB:           openLoreDB,
		ResolveProj:      resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  recordMCPTelemetry,
		RecordMiss:       recordMCPMiss,
		PrependNarration: true,
		EvaluateHints:    hintsBridge(),
	}
}

// registerBootstrap wires the tools that agents MUST be able to call
// before any active project exists: session start, mid-session project
// switch, and guild_status (re-orientation without re-bootstrapping).
func registerBootstrap(s *sdkmcp.Server) {
	registerSessionStart(s)
	registerSetProject(s)
	registerGuildStatus(s)
}

// registerAlwaysOn wires all guild tools. Full surface advertised at init.
func registerAlwaysOn(s *sdkmcp.Server) {
	// --- lore (read + write, common) ---
	loreDeps := buildMCPLoreDeps()
	lore.AppraiseCommand.BindMCP(s, loreDeps)
	lore.StudyCommand.BindMCP(s, loreDeps)
	lore.OathCommand.BindMCP(s, loreDeps)
	lore.ListCommand.BindMCP(s, loreDeps)
	lore.DossierCommand.BindMCP(s, loreDeps)
	lore.InscribeCommand.BindMCP(s, loreDeps)
	lore.ReforgeCommand.BindMCP(s, loreDeps)
	lore.UpdateCommand.BindMCP(s, loreDeps)
	lore.EchoesCommand.BindMCP(s, loreDeps)
	lore.WhispersCommand.BindMCP(s, loreDeps)
	lore.LinkCommand.BindMCP(s, loreDeps)    // provenance graph — write half
	lore.RipplesCommand.BindMCP(s, loreDeps) // provenance graph — read half
	// --- lore (hygiene) ---
	lore.InquestCommand.BindMCP(s, loreDeps)
	lore.MeldCommand.BindMCP(s, loreDeps)
	lore.CommuneCommand.BindMCP(s, loreDeps)
	lore.SealCommand.BindMCP(s, loreDeps)
	lore.CatalogCommand.BindMCP(s, loreDeps)
	// --- quest (common flow) ---
	mcpDeps := buildMCPCommandDeps()
	quest.PostCommand.BindMCP(s, mcpDeps)
	quest.UpdateCommand.BindMCP(s, mcpDeps)
	quest.AcceptCommand.BindMCP(s, mcpDeps)
	quest.JournalCommand.BindMCP(s, mcpDeps)
	quest.CampfireCommand.BindMCP(s, mcpDeps)
	quest.FulfillCommand.BindMCP(s, mcpDeps)
	// quest_clear is kept as a backward-compat MCP alias (same handler,
	// different tool name) so agents trained on the pre-QUEST-106 verb
	// still work. Tool discovery surfaces both; new agents prefer fulfill.
	quest.ClearCommand.BindMCP(s, mcpDeps)
	quest.BriefCommand.BindMCP(s, mcpDeps)
	quest.SummonCommand.BindMCP(s, mcpDeps)
	quest.OrdersCommand.BindMCP(s, mcpDeps)
	registerQuestBounties(s)
	quest.ListCommand.BindMCP(s, mcpDeps)
	quest.ScrollCommand.BindMCP(s, mcpDeps)
	quest.PulseCommand.BindMCP(s, mcpDeps)
	quest.GuildCommand.BindMCP(s, mcpDeps)
	quest.EpicCommand.BindMCP(s, mcpDeps)
	quest.ActiveCommand.BindMCP(s, mcpDeps)
	quest.ForfeitCommand.BindMCP(s, mcpDeps)
	// archive/restore is CLI-only (QUEST-45) — see tools_guild.go comment.
}
