package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/release"
	"github.com/mathomhaus/guild/internal/sleep"
)

// binaryVersion is the ldflags-stamped version string injected by
// cmd/guild/mcp.go via SetBinaryVersion. Defaults to "dev" so MCP tests
// that never call SetBinaryVersion see a stable, non-semver sentinel that
// suppresses the nudge (IsNewer returns false, nil for "dev").
var binaryVersion = "dev"

// SetBinaryVersion wires the ldflags-stamped version into the MCP layer
// so guild_session_start can emit an upgrade nudge when appropriate.
// Called from cmd/guild/mcp.go init(), before the first MCP session.
func SetBinaryVersion(v string) { binaryVersion = v }

// sleepNarration loads the one-line "while you slept" summary by
// consuming (claim + summarize, sleep.Narrate) every completed,
// unnarrated sleep pass in the shared lore DB. Returns "" when there is
// nothing to narrate. Package-level func var so tests can swap in a
// stub without seeding a journal.
//
// All failures degrade to "": narration must never break session_start
// on a cold, locked, or pre-migration DB (a journal-less DB reads as
// "no passes"): the same resilience contract as the embedder-health
// line in renderBounties. The narration reads only the shared DB, so it
// behaves identically whether session_start is served in-process or by
// the daemon.
var sleepNarration = func(ctx context.Context) string {
	loreDB, err := openLoreDB(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = loreDB.Close() }()
	line, err := sleep.Narrate(ctx, loreDB, time.Time{})
	if err != nil {
		return ""
	}
	return line
}

// mcpProjectResolver is the Resolver used by handleSessionStart when the
// agent omits the project arg and we need to auto-infer from the MCP
// server's cwd. Package-level so tests can swap in fake Getwd /
// GitToplevel seams without exec'ing real git.
//
// Production default wires to os.Getwd and real git; tests assign a
// Resolver{} literal with stubbed seams.
var mcpProjectResolver = project.DefaultResolver

// sessionStartInput is the typed input for the [guild_session_start]
// tool. Struct-tag-driven schema: [mcp.AddTool]'s generic variant reads
// `json` + `jsonschema` tags to produce the JSON Schema agents see.
//
// Description copy is intentionally lean — the WHAT/WHEN lives here; the
// discipline (why to call this first, what it loads) lives in the
// INSTRUCTIONS string loaded once at session start. Keep this struct
// deliberately small so the other tools can follow the same pattern
// without repeating the same workflow guidance.
type sessionStartInput struct {
	// BriefOnly narrows the response to the narration header plus the
	// last briefing, skipping the oath, echoes, bounty, parallelism,
	// and board-summary sections. Same tag pair as
	// questBountiesInput.BriefOnly and guildStatusInput.BriefOnly so
	// all three tools present one consistent arg.
	BriefOnly bool `json:"brief_only,omitempty" jsonschema:"when true, return only the last briefing"`

	// Project is the directory basename of the project to activate.
	// Optional — when omitted, the handler auto-infers the project by
	// running `git rev-parse --show-toplevel` in the MCP server's cwd
	// and looking the result up in the projects table. An explicit
	// value always wins over the inferred one.
	Project string `json:"project,omitempty" jsonschema:"directory basename, e.g. 'guild'. Optional — when omitted, auto-infers from the MCP server's cwd via git-toplevel lookup. Pass explicitly to override or when cwd auto-inference can't reach the project. Auto-infer handles git worktrees — falls back to the main-repo path if the worktree path isn't registered."`
}

// sessionStartTool is the [mcp.Tool] descriptor for guild_session_start.
// Declared as a package-level value so server_test.go can assert on the
// registered tool shape without reaching into the SDK's feature set.
//
// The Description covers WHAT it does and WHEN to use it. The full
// "why this is mandatory before other tools" discipline lives in
// INSTRUCTIONS, loaded once per session rather than repeated per-tool.
var sessionStartTool = &sdkmcp.Tool{
	Name: "guild_session_start",
	Description: "Call FIRST — before any other guild tool will work. " +
		"Sets the active project, loads the briefing, oath, and top " +
		"task, and defaults later guild tools to it (you do not need " +
		"to pass project to them again).",
}

// registerSessionStart wires guild_session_start onto s. Package-
// private so Serve and NewServer are the only public construction
// paths.
//
// The handler closes over the server core only: all persistence goes
// through the core's SessionStore (per-PID file at
// ~/.guild/sessions/<pid>.json on the default path) and bounties/oath
// payloads are resolved on every call from the project's registered DBs.
func (c *serverCore) registerSessionStart(s *sdkmcp.Server) {
	sdkmcp.AddTool(s, sessionStartTool, c.handleSessionStart)
}

// handleSessionStart is the tool handler. Thin: validate input,
// persist active project, then load the live bounties + oath snapshot
// through quest.Bounties + lore.Oath so the agent receives the real
// briefing + top task on the very first call.
//
// The ctx parameter is threaded through the session-store save path and
// every downstream DB call so a client-side cancellation aborts the
// whole bootstrap.
func (c *serverCore) handleSessionStart(
	ctx context.Context,
	_ *sdkmcp.CallToolRequest,
	in sessionStartInput,
) (*sdkmcp.CallToolResult, any, error) {
	start := time.Now()
	var handlerErr error
	var respBytes uint
	//nolint:gocritic // ptrToRefParam — defer reads late-bound values
	defer c.recordMCPTelemetry(ctx, "guild_session_start", start, &handlerErr, &respBytes)
	// Tell the hint engine we saw a session-start so the no-session-start
	// rule suppresses for this session.
	c.providers.recordHintsBootstrap("guild_session_start")

	name := strings.TrimSpace(in.Project)
	var viaWorktreeFallback bool
	if name == "" {
		// Auto-infer from the session's cwd via git-toplevel lookup:
		// the MCP server's own cwd on the stdio path, the shim's
		// preamble cwd on the daemon path (core.inferProject routes
		// the distinction). An explicit project arg would have taken
		// the fast path above; we only reach here when the agent
		// called guild_session_start() with no args. The inference
		// pipeline is the same one the CLI uses (project.Resolve),
		// reused so MCP and CLI can't diverge on which cwd resolves
		// to which project.
		inferred, fallback, err := c.inferProject(ctx)
		if err != nil {
			// Recoverable: the agent can fix by passing project=... or
			// by registering the project with guild init. Surface the
			// resolver's message verbatim (it already lists registered
			// project names when the cwd-path isn't registered).
			return toolErrorf(
				"no project arg given and auto-inference from cwd failed: %v — "+
					"pass guild_session_start(project='<directory-name>') explicitly, "+
					"or run 'guild init' in the current project to register it.",
				err,
			), nil, nil
		}
		name = inferred
		viaWorktreeFallback = fallback
	}

	if err := c.sessions.SetActiveProject(ctx, name); err != nil {
		// Persist failures are fatal from the agent's POV: retrying
		// won't help if the home directory is unwritable. Surface the
		// OS-level detail so the user (not the agent) can fix it.
		return toolFatalf("persist active project %q: %v", name, err), nil, nil
	}

	// Narrate the state change inline so the host's collapsed-MCP UX
	// doesn't hide it from the user. When the worktree fallback fired,
	// append a suffix so the agent and user see how the project was
	// resolved (worktree cwd to main-repo path to project registration).
	header := fmt.Sprintf("📍 active project: %s", name)
	if viaWorktreeFallback {
		header += " (inferred from worktree's main-repo path)"
	}

	// Try to load the bounties snapshot. Graceful-degradation: if the
	// project isn't registered yet (fresh `guild init` path) or either
	// DB is unreachable, we still return success with the narration
	// header — the agent can proceed and register via lore/quest init.
	//
	// renderBounties always returns a non-empty structural block:
	// section headers render even on an empty project so first-time
	// users see the shape (QUEST-1).
	body, bountiesRes := renderBounties(ctx, name, in.BriefOnly)
	if body == "" {
		body = emptyBountiesSkeleton()
	}

	// Board-summary line: counts at a glance for Codex-class collapsed views.
	// Suppressed in brief-only mode: the oath/echo loaders and the bounty
	// scan are skipped there, so the counts would all read zero regardless
	// of the real board state.
	var boardLine string
	if bountiesRes != nil && !in.BriefOnly {
		oathCount := len(bountiesRes.Oath)
		bountyCount := len(bountiesRes.AllNext)
		echoCount := len(bountiesRes.Echoes)
		boardLine = fmt.Sprintf("📊 board: %d oaths, %d bounties, %d echoes\n", oathCount, bountyCount, echoCount)
	}

	// Presence line (ADR-005 Phase 3): the FIRST intentional daemon-only
	// additive output in session_start. When this server is served inside a
	// running daemon (c.presence non-nil), append exactly one line reporting
	// how many agents are live and, when any hold a lease, how many are on
	// lease, sourced from the in-memory registry snapshot (no db round-trip).
	// Suppressed in brief-only mode, which carries no board context. On the
	// stdio and in-process fallback paths there is no registry, so
	// c.presence is nil and no line is emitted: session_start stays
	// byte-identical to today, which the daemon parity suite enforces (it
	// strips this one documented daemon-only prefix from the daemon-up arm
	// before the byte-for-byte comparison).
	var presenceLine string
	if c.presence != nil && !in.BriefOnly {
		presenceLine = formatPresenceLine(c.presence.Presence()) + "\n"
	}

	// "While you slept" narration: consume any completed, unnarrated
	// sleep passes and surface them as one moon-prefixed line directly
	// after the board summary (directly after the header in brief-only
	// mode, which has no board line), before the briefing body, so
	// collapsed-view clients still show it near the top. Empty when the
	// journal has nothing to narrate, keeping the never-slept output
	// byte-identical.
	var sleepLine string
	if line := sleepNarration(ctx); line != "" {
		sleepLine = line + "\n"
	}

	full := header + "\n" + boardLine + presenceLine + sleepLine + "\n" + body
	respBytes = uint(len(full))

	// Degraded no-daemon fallback (ADR-005 Part 2): fire one bounded,
	// once-per-process sleep autopass in the background. It is gated to a
	// no-daemon process (a resident daemon owns the idle scheduler) and
	// throttled against the journal, returns immediately, and never
	// delays this response: the result is fully assembled above. When
	// sleep is disabled or a daemon is up this is a no-op.
	//nolint:contextcheck // the autopass goroutine is server-lifetime work and intentionally uses context.Background() internally, mirroring the auto-backfill trigger.
	maybeTriggerSleepAutopass(c.providers.embed, newLogger())

	return textResult(full), nil, nil
}

// formatPresenceLine renders the daemon-only "N agents active" line from an
// in-memory registry presence snapshot. The agent count includes the
// calling session itself (it is registered before its session_start runs),
// so a lone agent reads "1 agent active". When at least one session holds a
// lease, an on-lease clause is appended ("N agents active, M on lease"),
// surfacing the liveness Phase 3 exists for; with no leases held the clause
// is omitted so the common case stays terse. The count is singularized for
// readability.
func formatPresenceLine(p daemon.Presence) string {
	line := fmt.Sprintf("👥 %s active", pluralAgents(p.Sessions))
	if p.OnLease > 0 {
		line += fmt.Sprintf(", %d on lease", p.OnLease)
	}
	return line
}

// pluralAgents renders the agent count with a singular/plural noun
// ("1 agent" / "2 agents").
func pluralAgents(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}

// renderBounties loads the briefing + oath + top task for projectID
// and returns a compact text block plus the raw BountiesResult so the
// caller can inspect counts (oaths, bounties, echoes) without a second
// DB round-trip. Both return values are nil/empty when no data is
// available; the caller supplies a friendly fallback.
//
// Failures are swallowed: bootstrap should never hard-error on a cold
// DB, because the agent's recovery path (init the project) depends on
// reaching the post-bootstrap tool surface.
func renderBounties(ctx context.Context, projectID string, briefOnly bool) (string, *quest.BountiesResult) {
	questDB, err := openQuestDB(ctx)
	if err != nil {
		return "", nil
	}
	defer func() { _ = questDB.Close() }()

	// Optional lore wiring: if the lore DB is unreachable we still
	// surface quest bounties without oath/echoes. Matches the CLI's
	// graceful-degradation pattern in quest_agent.go. Skipped entirely
	// in brief-only mode: the oath/echo/health reads would be discarded
	// there, so the lore DB is never opened (previously a brief-only
	// call opened the handle and leaked it, since the deferred close
	// only ran on the full path).
	var oathLoader quest.OathLoader
	var echoLoader quest.EchoLoader
	var embedderHealthLine string
	if !briefOnly {
		if loreDB, loreErr := openLoreDB(ctx); loreErr == nil {
			defer func() { _ = loreDB.Close() }()
			oathLoader = func(ctx context.Context, proj string) ([]quest.OathEntry, error) {
				entries, err := lore.Oath(ctx, loreDB, proj)
				if err != nil {
					return nil, err
				}
				out := make([]quest.OathEntry, len(entries))
				for i, e := range entries {
					out[i] = quest.OathEntry{Title: e.Title, Summary: e.Summary}
				}
				return out, nil
			}
			echoLoader = func(ctx context.Context, proj string) ([]quest.EchoEntry, error) {
				echoes, err := lore.Echoes(ctx, loreDB, proj, false)
				if err != nil {
					return nil, err
				}
				out := make([]quest.EchoEntry, len(echoes))
				for i, e := range echoes {
					out[i] = quest.EchoEntry{Title: e.Entry.Title, Reason: e.Reason}
				}
				return out, nil
			}

			// Read the embedder health line. Failures are swallowed: a missing
			// meta table (pre-migration DB) must not break session-start.
			// Only non-healthy states emit a line; healthy state returns "".
			if report, hErr := embed.ReadHealthReport(ctx, loreDB, embed.LoreCorpus{}); hErr == nil {
				embedderHealthLine = report.SessionLine()
			}
		}
	}

	res, err := quest.Bounties(ctx, questDB, projectID, briefOnly, oathLoader, echoLoader)
	if err != nil {
		// Project not registered yet, or no quests posted: degrade
		// cleanly rather than bubbling a bootstrap failure.
		return "", nil
	}
	body := formatBounties(res, briefOnly)
	if embedderHealthLine != "" {
		body += "\n" + embedderHealthLine + "\n"
	}
	// Upgrade nudge: check for a newer release and append a line when one
	// is available. Silent on all failures; zero-allocation no-op when
	// up-to-date or when GUILD_NO_UPDATE_CHECK=1.
	if nudge := release.CheckAndNudgeMCP(ctx, binaryVersion); nudge != "" {
		body += "\n" + nudge + "\n"
	}
	return body, res
}

// formatBounties renders a BountiesResult as the text block the
// session-start handler returns. Every section header
// (briefing / oath / echoes / bounties / parallelism) is printed
// unconditionally so first-time users see the full shape even on an
// empty project (QUEST-1); populated sections render their contents
// underneath, empty sections render "(none yet)" placeholders with a
// one-line hint for how to populate them.
//
// Matches the CLI's emoji-prefixed output where it's load-bearing but
// omits the more verbose CLI decorations that don't help agents.
func formatBounties(res *quest.BountiesResult, briefOnly bool) string {
	var b strings.Builder

	// Section 1: last briefing.
	if res.LastBriefText != "" {
		b.WriteString(fmt.Sprintf("📋 last briefing [%s] by %s:\n",
			res.LastBriefAt, res.LastBriefAgent))
		b.WriteString("  ")
		b.WriteString(res.LastBriefText)
		b.WriteString("\n\n")
	} else {
		b.WriteString("📋 last briefing: (none yet) — call quest_brief before session end to leave a note for the next agent.\n\n")
	}

	if briefOnly {
		return strings.TrimRight(b.String(), "\n")
	}

	// Section 2: oath (principles).
	if len(res.Oath) > 0 {
		b.WriteString(fmt.Sprintf("⚔️ %d oath(s):\n", len(res.Oath)))
		for _, o := range res.Oath {
			b.WriteString("  ")
			b.WriteString(o.Title)
			b.WriteString(" — ")
			b.WriteString(o.Summary)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	} else {
		b.WriteString("⚔️ oath: (none yet) — inscribe principles with kind=\"principle\" and they auto-load here.\n\n")
	}

	// Section 3: fading echoes (stale lore).
	if len(res.Echoes) > 0 {
		b.WriteString(fmt.Sprintf("👻 %d fading echo(es):\n", len(res.Echoes)))
		for _, e := range res.Echoes {
			b.WriteString("  ")
			b.WriteString(e.Title)
			b.WriteString(" [")
			b.WriteString(e.Reason)
			b.WriteString("]\n")
		}
		b.WriteString("\n")
	} else {
		b.WriteString("👻 fading echoes: (none) — stale research/decision entries surface here as they age out.\n\n")
	}

	// Section 4: top bounty.
	switch {
	case res.NoUnclaimed:
		b.WriteString("🎯 bounties: ✅ no unclaimed tasks — all done, blocked, or in progress.\n")
	case res.TopQuest != nil:
		q := res.TopQuest
		b.WriteString("🎯 top bounty:\n")
		b.WriteString("  ")
		b.WriteString(q.ID)
		if q.Priority != "" {
			b.WriteString(" [")
			b.WriteString(string(q.Priority))
			b.WriteString("]")
		}
		if q.Epic != "" {
			b.WriteString(" · ")
			b.WriteString(q.Epic)
		}
		b.WriteString("\n  ")
		b.WriteString(q.Subject)
		b.WriteString("\n")
		if len(q.Files) > 0 {
			b.WriteString("  files: ")
			b.WriteString(strings.Join(q.Files, ", "))
			b.WriteString("\n")
		}
		for _, a := range q.Acceptance {
			b.WriteString("  ✓ ")
			b.WriteString(a)
			b.WriteString("\n")
		}
		b.WriteString("→ call quest_accept(quest_id=\"")
		b.WriteString(q.ID)
		b.WriteString("\")")
	default:
		// Project registered but no quests posted at all.
		b.WriteString("🎯 bounties: (none yet) — call quest_post to seed the board.")
	}

	// Section 5: parallelism.
	switch {
	case res.ParallelCount > 0 && res.TopQuest != nil:
		b.WriteString(fmt.Sprintf("\n\n⚡ %d task(s) can run in parallel with %s:\n",
			res.ParallelCount, res.TopQuest.ID))
		for _, pair := range res.ParallelPairs {
			b.WriteString("  · ")
			b.WriteString(pair.B)
			b.WriteString("\n")
		}
	default:
		b.WriteString("\n\n⚡ parallelism: (none available) — additional unclaimed quests with no file overlap and no dep conflict will surface here.\n")
	}

	return b.String()
}

// inferProjectFromCWD auto-resolves the active project when the agent
// called guild_session_start() with no arg. Opens the quest DB
// read-only-ish (shared with other handlers via openQuestDB) and hands
// off to project.ResolveFull, which runs the standard CLI lookup order:
// git-toplevel of cwd → LookupByPath in the projects table, with a
// git-common-dir worktree fallback if the worktree path isn't registered.
// Errors propagate verbatim — project.ResolveFull's message already lists
// registered project names when the cwd isn't among them, which is the
// most useful recovery hint for the agent.
//
// The second return value is true when the resolution succeeded only via
// the worktree fallback (cwd is a linked worktree; the main-repo path was
// registered). Callers use this to vary narration.
//
// The resolver is indirected through mcpProjectResolver so tests can
// swap in fake Getwd / GitToplevel / GitCommonDir seams.
func inferProjectFromCWD(ctx context.Context) (projectID string, viaFallback bool, err error) {
	return inferProjectFromDir(ctx, "")
}

// inferProjectFromDir is inferProjectFromCWD generalized over the
// directory the inference anchors on. dir == "" preserves the
// resolver's own Getwd (the stdio server resolves against its host
// process's cwd); a non-empty dir substitutes for Getwd entirely (the
// daemon resolves each connection against the shim's preamble cwd, so
// two sessions from different project checkouts never bleed into each
// other through the daemon's ambient working directory).
func inferProjectFromDir(ctx context.Context, dir string) (projectID string, viaFallback bool, err error) {
	db, err := openQuestDB(ctx)
	if err != nil {
		return "", false, fmt.Errorf("open quest db: %w", err)
	}
	defer func() { _ = db.Close() }()

	resolver := mcpProjectResolver
	if dir != "" {
		resolver.Getwd = func() (string, error) { return dir, nil }
	}
	res, err := resolver.ResolveFull(ctx, db, "")
	if err != nil {
		return "", false, err
	}
	return res.Project.ID, res.ViaWorktreeFallback, nil
}

// emptyBountiesSkeleton is the absolute-cold-start fallback shown when
// the quest DB can't even be opened (fresh install, no `guild init`
// run yet). Mirrors the five section headers so first-time users see
// the same shape they'll see once the project is registered.
func emptyBountiesSkeleton() string {
	var b strings.Builder
	b.WriteString("no bounties yet — run lore init / quest init to register the project, or post your first quest.\n\n")
	b.WriteString("📋 last briefing: (none yet)\n")
	b.WriteString("⚔️ oath: (none yet)\n")
	b.WriteString("👻 fading echoes: (none)\n")
	b.WriteString("🎯 bounties: (none yet)\n")
	b.WriteString("⚡ parallelism: (none available)\n")
	return b.String()
}
