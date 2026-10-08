package mcp

import (
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/module"
	_ "github.com/mathomhaus/guild/internal/modules" // activate core modules (quest/lore/session) in the registry
)

// loreValidDaysFromConfig loads the merged config and returns the
// per-kind valid_days windows ([inscribe.valid_days]) for lore writes.
// Returns nil on load failure so the built-in kind defaults apply: a
// broken config file must never block an inscribe (matching the
// swallow-and-degrade posture of recordMCPTelemetry). Wired as
// command.Deps.LoreValidDays and therefore called lazily per tool
// invocation, so the long-lived server observes config edits without a
// restart.
func loreValidDaysFromConfig() map[string]int {
	cfg, err := config.Load(nil)
	if err != nil {
		return nil
	}
	return cfg.Inscribe.ValidDays
}

// moduleEnabledPredicate loads the merged config and returns the
// predicate module.Enabled consults to decide which modules wire their
// tools onto the MCP surface (ADR-006 Phase 3). On a config-load failure
// it returns nil, which module.Enabled treats as "every module on its own
// DefaultEnabled()" — a broken config file must never silently strip the
// core tools, matching the swallow-and-degrade posture of
// loreValidDaysFromConfig above. Called once per server construction, so
// a long-lived MCP server reflects the config present at connect time.
func moduleEnabledPredicate() func(name string, def bool) bool {
	cfg, err := config.Load(nil)
	if err != nil {
		return nil
	}
	return config.ModuleEnabled(cfg)
}

// Register wires every guild tool onto s against a default per-process
// core: per-PID session identity plus a fresh provider bundle, exactly
// what the stdio server runs with. Hosts that need injectable seams
// (explicit session identity, shared provider bundle) construct through
// NewServer instead, which routes the same registration with their
// options. bootstrap → always-on; no deferred tier.
func Register(s *sdkmcp.Server) {
	// Reset the once-per-process sleep-autopass gate so each default
	// server construction starts with a fresh gate, mirroring how the
	// auto-backfill gate is reset for test-spawned servers. Production
	// builds one server per process, so this runs once; tests that build
	// several servers via Register each get a clean gate.
	resetSleepAutopassState()
	registerAll(s, &serverCore{
		sessions:  processSessionStore{},
		providers: newProcessProviders(),
	})
}

// registerAll wires every guild tool onto s against core. Called once
// per constructed server (NewServer or Register); every handler closure
// and Deps bundle built here references this core and no package-level
// state, so two servers in one process cannot clobber each other.
func registerAll(s *sdkmcp.Server, core *serverCore) {
	core.registerBootstrap(s)
	core.registerAlwaysOn(s)
}

// buildMCPCommandDeps constructs the quest-side Deps bundle. The
// registry's OpenDB opens quest.db; ResolveProj uses the auto-bootstrap
// resolver so MCP reconnects are invisible (QUEST-65). RecordTelemetry
// wires the MCP usage.log emitter so every tool call produces a row.
// PrependNarration enables the auto-bootstrap narration path in the MCP
// handler wrapper: when auto-bootstrap fires, the narration line is
// prepended to the tool's output body. OpenLoreDB is wired so
// quest_post's spec= param (QUEST-63) can atomically inscribe a
// kind=decision lore entry alongside the quest.
// Embed carries the questEmbedProvider so quest_search can reach the
// RRF arm when meta.quest.embedder_state="enabled" (QUEST-258).
func (c *serverCore) buildMCPCommandDeps() command.Deps {
	d := command.Deps{
		OpenDB:           openQuestDB,
		ResolveProj:      c.resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  c.recordMCPTelemetry,
		PrependNarration: true,
		OpenLoreDB:       openLoreDB,
		EvaluateHints:    c.providers.hintsBridge(),
		LoreValidDays:    loreValidDaysFromConfig,
	}
	if c.providers.questEmbed != nil {
		d.Embed = c.providers.questEmbed
	}
	// Lease is the daemon's per-session quest-lease port (ADR-005 Phase
	// 3): an accept under this session writes a lease, a mutating call
	// renews it. Nil for the stdio and in-process paths, the
	// byte-identical no-daemon contract. The field is `any` and only set
	// when genuinely non-nil so a typed nil never defeats the
	// leaseFromDeps nil-check (same care as Embed above).
	if c.lease != nil {
		d.Lease = c.lease
	}
	return d
}

// buildMCPLoreDeps is the lore-side sibling. Identical ResolveProj with
// auto-bootstrap (QUEST-65), but OpenDB opens lore.db. RecordMiss wires
// the misses.log emitter for lore_appraise zero-result queries. Embed
// is the lazy-reconstruct provider (QUEST-219, ADR-003 Phase 1); the
// lore side calls .ResolveEmbedDeps(ctx) on every handler entry so a
// mid-session meta flip is observed without restarting the MCP
// server. A nil pointer returned from the provider is the Phase-0
// BM25+stopwords fallback and every lore handler tolerates it.
//
// Note: command.Deps.Embed is declared `any`, so a typed-nil
// *embedProvider would become a non-nil interface value and defeat
// the nil-check in embedFromDeps. Only assign the field when the
// provider is genuinely non-nil.
func (c *serverCore) buildMCPLoreDeps() command.Deps {
	d := command.Deps{
		OpenDB:           openLoreDB,
		OpenQuestDB:      openQuestDB,
		ResolveProj:      c.resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  c.recordMCPTelemetry,
		RecordMiss:       recordMCPMiss,
		PrependNarration: true,
		EvaluateHints:    c.providers.hintsBridge(),
		LoreValidDays:    loreValidDaysFromConfig,
	}
	if c.providers.embed != nil {
		d.Embed = c.providers.embed
	}
	return d
}

// registerBootstrap wires the tools that agents MUST be able to call
// before any active project exists: session start, mid-session project
// switch, and guild_status (re-orientation without re-bootstrapping).
func (c *serverCore) registerBootstrap(s *sdkmcp.Server) {
	c.registerSessionStart(s)
	c.registerSetProject(s)
	c.registerGuildStatus(s)
}

// registerAlwaysOn wires all guild tools. Full surface advertised at init.
//
// ADR-006 Phase 2 cutover: the ~40-line hand-maintained BindMCP list is
// replaced by a loop over the module registry. For each enabled module the
// kernel resolves that module's MCP-side Deps bundle (the loreDeps vs
// quest/mcpDeps distinction is preserved, keyed by module name) and binds
// every Command the module contributes. command.BindMCP skips CLIOnly specs
// itself, so MCPOnly aliases like quest_clear still register and CLI-only
// verbs do not. Tool order is not observable: the MCP SDK sorts tools/list
// alphabetically, so the advertised surface is byte-identical regardless of
// bind order.
//
// ADR-006 Phase 3: module.Enabled is now driven by the config-backed
// predicate (moduleEnabledPredicate), so a [modules] toggle, a
// GUILD_MODULE_<NAME> / GUILD_NO_<NAME> env override, or a --module flag
// removes a disabled module's tools from the surface entirely. With a
// silent config every module stays on its own DefaultEnabled() (the
// quest+lore+session default set), byte-identical to the Phase 2 nil
// predicate.
func (c *serverCore) registerAlwaysOn(s *sdkmcp.Server) {
	for _, m := range module.Enabled(moduleEnabledPredicate()) {
		deps, ok := c.mcpDepsForModule(m.Name())
		if !ok {
			// A module with no MCP-side Deps bundle contributes no MCP
			// tools through this loop (session today: its bootstrap tools
			// are hand-wired in registerBootstrap). Skip it rather than
			// bind its (possibly nil) Commands against an empty Deps.
			continue
		}
		for _, cmd := range m.Commands() {
			cmd.BindMCP(s, deps)
		}
	}

	// quest_bounties is a hand-wired bootstrap-adjacent tool (not a
	// command.Command), so it is registered outside the module loop. Its
	// position does not affect the sorted tools/list surface.
	c.registerQuestBounties(s)
	// archive/restore is CLI-only (QUEST-45) — see tools_guild.go comment.
}

// mcpDepsForModule returns the MCP-side command.Deps bundle a module's
// Commands should bind with, keyed by module name. This preserves the
// pre-cutover loreDeps vs quest/mcpDeps split: lore commands open lore.db
// and wire RecordMiss + the lore embed provider; quest commands open
// quest.db and wire OpenLoreDB + the quest embed provider + the daemon
// lease port. ok=false means the module contributes no MCP tools (session),
// so the caller skips it.
func (c *serverCore) mcpDepsForModule(name string) (command.Deps, bool) {
	switch name {
	case "lore":
		return c.buildMCPLoreDeps(), true
	case "quest":
		return c.buildMCPCommandDeps(), true
	case "eval":
		return c.buildMCPEvalDeps(), true
	case "session":
		// session's bootstrap tools are hand-wired in registerBootstrap.
		return command.Deps{}, false
	default:
		// A non-core enabled module (e.g. compression) whose verbs need no
		// database. Bind with a minimal Deps: project resolution + clock +
		// telemetry, but no DB openers, since its handlers never call OpenDB.
		// This branch is only reached for an enabled module, so a disabled
		// compression module contributes no MCP tools and the default tool
		// list stays byte-identical.
		return command.Deps{
			ResolveProj:      c.resolveProjectAutoBootstrap,
			Now:              time.Now,
			RecordTelemetry:  c.recordMCPTelemetry,
			PrependNarration: true,
		}, true
	}
}

// buildMCPEvalDeps constructs the MCP-side Deps for the eval module (ADR-006
// Phase 6). The eval_run handler is self-contained — it seeds and tears down
// its own isolated in-memory corpus and never touches the real databases — so
// it needs no OpenDB / ResolveProj / OpenLoreDB. We wire only Now and the
// usage.log telemetry emitter so an eval_run call is logged like any other
// tool. The module is off by default, so this Deps is only ever built when an
// operator has explicitly enabled eval.
func (c *serverCore) buildMCPEvalDeps() command.Deps {
	return command.Deps{
		Now:             time.Now,
		RecordTelemetry: c.recordMCPTelemetry,
	}
}
