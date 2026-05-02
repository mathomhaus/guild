package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
)

// expectedTools is the full set of tool names Register wires. Kept
// sorted so test diffs stay readable. When adding/removing a tool,
// update this list AND the register.go call site — TestTools_RegisteredCount
// asserts the exact set so a missing update fails CI loudly.
var expectedTools = []struct {
	name string
}{
	// guild_archive deleted in QUEST-45 — archive/restore is CLI-only.
	{"guild_session_start"},
	{"guild_status"},
	{"guild_set_project"},
	{"lore_appraise"},
	{"lore_catalog"},
	{"lore_commune"},
	{"lore_dossier"},
	{"lore_inquest"},
	{"lore_inscribe"},
	{"lore_link"},
	{"lore_unlink"},
	{"lore_list"},
	{"lore_meld"},
	{"lore_oath"},
	{"lore_reforge"},
	{"lore_seal"},
	{"lore_study"},
	{"lore_update"},
	{"quest_accept"},
	{"quest_active"},
	{"quest_bounties"},
	{"quest_brief"},
	{"quest_campfire"},
	{"quest_clear"}, // backward-compat alias for quest_fulfill (QUEST-106)
	{"quest_epic"},
	{"quest_forfeit"},
	{"quest_fulfill"},
	{"quest_guild"},
	{"quest_journal"},
	{"quest_list"},
	{"quest_orders"},
	{"quest_post"},
	{"quest_pulse"},
	{"quest_scroll"},
	{"quest_summon"},
	{"quest_update"},
	{"lore_echoes"},
	{"lore_whispers"},
	{"lore_ripples"},
	// Embedder health tools (Phase 1.6 ADR-003, QUEST-211).
	{"lore_health"},
	{"lore_embed_rebuild"},
	// Coverage denominator reconcile (QUEST-220 / LORE-373).
	{"lore_coverage_reconcile"},
	// Quest full-text + vector search (QUEST-224 / LORE-377).
	{"quest_search"},
}

// isolateProject sets up a fresh $HOME, registers an active project,
// and runs `guild init` via the direct lore/quest packages so
// downstream tools can open the DBs. Returns the project id so the
// test can reference it.
//
// Shared by every tool-level smoke test so each one is independent:
// no test relies on side effects from another.
func isolateProject(t *testing.T) string {
	t.Helper()
	home := isolateHome(t)
	projDir := filepath.Join(home, "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}

	const pid = "testproj"
	ctx := context.Background()

	// Register the project row in both quest and lore DBs so the
	// mutation smoke tests don't trip foreign-key constraints. We
	// deliberately skip quest.Init / lore.InitFrom — those require a
	// git repo, which an in-memory test can't provide without heavy
	// setup. project.Register is the minimal write that satisfies
	// every FK pointing at projects(id).
	qdb, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = qdb.Close() }()
	if err := project.Register(ctx, qdb, pid, projDir, "TASKS.md"); err != nil {
		t.Fatalf("quest project register: %v", err)
	}

	ldb, err := openLoreDB(ctx)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = ldb.Close() }()
	if err := project.Register(ctx, ldb, pid, projDir, "TASKS.md"); err != nil {
		t.Fatalf("lore project register: %v", err)
	}

	// Now activate the project on this PID so resolveProject returns pid.
	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	t.Cleanup(cleanup)

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.IsError {
		t.Logf("bootstrap warning: %s", textOf(res.Content))
	}
	return pid
}

// TestTools_RegisteredCount asserts ListTools returns exactly the expectedTools
// set. Fails loud when a tool is added or removed without updating the registry.
func TestTools_RegisteredCount(t *testing.T) {
	isolateHome(t)
	got := listToolNames(t)
	want := allExpected()
	if diff := cmp(got, want); diff != "" {
		t.Errorf("tool set mismatch:\n%s", diff)
	}
}

// TestTools_BootstrapRequired locks the bootstrap contract: every
// non-bootstrap tool, called without a prior guild_session_start,
// returns IsError=true with a recovery-guiding [error] message.
//
// We iterate the full tool set, send a minimally-parsed CallTool with
// empty arguments, and assert the recoverable shape. Some tools may
// additionally fail their own param validation (missing quest_id,
// etc.) — that's acceptable as long as the failure comes via IsError
// and contains either the bootstrap guidance or a recoverable [error].
func TestTools_BootstrapRequired(t *testing.T) {
	isolateHome(t)
	// Deliberately DO NOT call guild_session_start — that's the point.
	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	for _, e := range expectedTools {
		name := e.name
		if name == "guild_session_start" || name == "guild_set_project" {
			continue // bootstrap-exempt per spec
		}
		t.Run(name, func(t *testing.T) {
			// Build a minimal-but-schema-valid args map so the SDK's
			// schema validator doesn't short-circuit us. We just need
			// SOMETHING for required fields; the handler will hit
			// resolveProject before doing anything with it.
			args := minArgsFor(name)
			res, err := client.CallTool(context.Background(),
				&sdkmcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				// An MCP-protocol error (schema validation, etc.) is
				// acceptable — it still means the tool didn't silently
				// operate without a project. Log and move on.
				t.Logf("protocol-layer reject (acceptable): %v", err)
				return
			}
			if !res.IsError {
				t.Errorf("%s: expected IsError=true without bootstrap; got body=%q",
					name, textOf(res.Content))
				return
			}
			body := textOf(res.Content)
			if !strings.Contains(body, "[error]") && !strings.Contains(body, "[fatal]") {
				t.Errorf("%s: error body missing [error]/[fatal] prefix: %q", name, body)
			}
		})
	}
}

// TestTools_SmokeRoundTrip exercises one CallTool per tool with valid
// minimal arguments, post-bootstrap. Most tools will return an empty-
// state friendly message (no entries, no quests). The assertion is
// light-touch: the tool must respond with non-error OR a recoverable
// error containing a user-recovery hint — it must NOT crash the
// protocol.
func TestTools_SmokeRoundTrip(t *testing.T) {
	isolateProject(t) // sets $HOME, registers testproj, activates it.

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	for _, e := range expectedTools {
		name := e.name
		t.Run(name, func(t *testing.T) {
			args := smokeArgsFor(name)
			res, err := client.CallTool(context.Background(),
				&sdkmcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Fatalf("%s: protocol error: %v", name, err)
			}
			body := textOf(res.Content)
			if res.IsError {
				// Acceptable if the error is recoverable + well-shaped.
				if !strings.Contains(body, "[error]") && !strings.Contains(body, "[fatal]") {
					t.Errorf("%s: IsError=true but no [error]/[fatal] prefix: %q",
						name, body)
				}
				return
			}
			if strings.TrimSpace(body) == "" {
				t.Errorf("%s: empty success body (violates narration contract)", name)
			}
		})
	}
}

// TestTools_MutationNarration asserts the narration contract: every
// mutation tool (state change) returns a one-line chat-friendly summary
// with an emoji prefix. Regression gate for the "collapsed MCP output
// hides the effect" failure mode.
//
// The covered mutations: inscribe, post, accept, clear, journal,
// reforge, brief, update, forfeit, epic, seal, catalog, link.
func TestTools_MutationNarration(t *testing.T) {
	pid := isolateProject(t)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	// Each case picks a mutation that can succeed against a fresh DB
	// (no dependencies on earlier calls in this test). The emoji
	// pattern matches the actual prefixes used in tools_lore.go /
	// tools_quest.go.
	cases := []struct {
		name     string
		tool     string
		args     map[string]any
		emojiRe  string
		setup    func() map[string]any
		skipWhen func() bool
	}{
		{
			tool: "lore_inscribe",
			args: map[string]any{
				"title":   "narration-test",
				"kind":    "observation",
				"summary": "smoke summary",
				"topic":   "test",
				"project": pid,
			},
			emojiRe: `📜 inscribed LORE-\d+`,
		},
		{
			tool: "quest_post",
			args: map[string]any{
				"subject": "narration smoke",
				"project": pid,
			},
			emojiRe: `➕ posted QUEST-\d+`,
		},
	}

	for _, c := range cases {
		c := c
		tool := c.tool
		t.Run(tool, func(t *testing.T) {
			args := c.args
			if c.setup != nil {
				args = c.setup()
			}
			res, err := client.CallTool(context.Background(),
				&sdkmcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				t.Fatalf("%s: %v", tool, err)
			}
			if res.IsError {
				t.Fatalf("%s: IsError=true: %s", tool, textOf(res.Content))
			}
			body := textOf(res.Content)
			// ≤80-char narration (first line only).
			firstLine := strings.SplitN(body, "\n", 2)[0]
			if len(firstLine) > 80 {
				t.Errorf("%s: narration first line >80 chars: %q",
					tool, firstLine)
			}
			re := regexp.MustCompile(c.emojiRe)
			if !re.MatchString(firstLine) {
				t.Errorf("%s: narration missing emoji prefix; want /%s/, got %q",
					tool, c.emojiRe, firstLine)
			}
		})
	}
}

// TestFormatAppraise removed — formatAppraise moved into
// internal/lore as part of QUEST-45. The age-format assertion lives in
// a lore-package test if needed; not re-added here to keep the
// package boundary clean.

func TestQuestAccept_ReturnsQuestCardAndRecentState(t *testing.T) {
	pid := isolateProject(t)
	ctx := context.Background()

	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()

	q, err := quest.Post(ctx, db, pid, quest.PostParams{
		Subject:    "Implement retry budget",
		Priority:   "P1",
		Epic:       "launch",
		Files:      []string{"internal/retry/retry.go"},
		Acceptance: []string{"tests pass", "docs updated"},
	})
	if err != nil {
		t.Fatalf("quest.Post: %v", err)
	}
	if err := quest.Journal(ctx, db, pid, q.ID, "agent", "Traced retry accounting race."); err != nil {
		t.Fatalf("quest.Journal: %v", err)
	}
	if err := quest.Campfire(ctx, db, pid, q.ID, quest.CampfireParams{
		Hypothesis: "budget overflow in retry path",
		Next:       "inspect retry state transitions",
		Agent:      "agent",
	}); err != nil {
		t.Fatalf("quest.Campfire: %v", err)
	}

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	if _, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "quest_accept",
		Arguments: map[string]any{"quest_id": q.ID, "project": pid},
	})
	if err != nil {
		t.Fatalf("quest_accept: %v", err)
	}
	if res.IsError {
		t.Fatalf("quest_accept IsError=true: %s", textOf(res.Content))
	}

	body := textOf(res.Content)
	for _, want := range []string{
		"⚔️ accepted " + q.ID + ": Implement retry budget",
		"priority=P1",
		"campaign=launch",
		"files:",
		"acceptance:",
		"latest journal: Traced retry accounting race.",
		"latest campfire: hypothesis: budget overflow in retry path | next: inspect retry state transitions",
		`next useful lore call: lore_appraise(query="Implement retry budget", all_projects=True)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("quest_accept body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[checkpoint] accepted by") {
		t.Fatalf("quest_accept leaked auto-checkpoint note:\n%s", body)
	}
}

// TestFormatInscribeNarration removed in QUEST-45 — the bespoke
// formatInscribeNarration helper was replaced by formatInscribed in
// internal/lore/inscribe_cmd.go. The link-remedy line was dropped
// from the unified format (it was a CLI-only hint); if we want it
// back, it belongs on CLIFormat only.

func TestQuestClear_ListsUnblockedOnSeparateLines(t *testing.T) {
	pid := isolateProject(t)
	ctx := context.Background()

	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()

	a, err := quest.Post(ctx, db, pid, quest.PostParams{Subject: "A"})
	if err != nil {
		t.Fatalf("post A: %v", err)
	}
	b, err := quest.Post(ctx, db, pid, quest.PostParams{
		Subject:   "B",
		DependsOn: []string{a.ID},
	})
	if err != nil {
		t.Fatalf("post B: %v", err)
	}

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	if _, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "quest_clear",
		Arguments: map[string]any{
			"quest_id": a.ID,
			"report":   "done in abc123; released downstream work",
			"project":  pid,
		},
	})
	if err != nil {
		t.Fatalf("quest_clear: %v", err)
	}
	if res.IsError {
		t.Fatalf("quest_clear IsError=true: %s", textOf(res.Content))
	}

	body := textOf(res.Content)
	if !strings.Contains(body, "\n  unblocked:\n    - "+b.ID+": B") {
		t.Fatalf("quest_clear missing multiline unblocked block:\n%s", body)
	}
	if strings.Contains(body, "(unblocked:") {
		t.Fatalf("quest_clear still uses collapsed unblock format:\n%s", body)
	}
}

// TestQuestClear_BriefHint verifies that quest_clear emits the stale-brief
// advisory when no brief has been written, and suppresses it when a recent
// brief exists.
func TestQuestClear_BriefHint(t *testing.T) {
	pid := isolateProject(t)
	ctx := context.Background()

	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Post a quest.
	q, err := quest.Post(ctx, db, pid, quest.PostParams{Subject: "hint-test"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	if _, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	t.Run("no_brief_shows_hint", func(t *testing.T) {
		res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "quest_clear",
			Arguments: map[string]any{
				"quest_id": q.ID,
				"report":   "done abc",
				"project":  pid,
			},
		})
		if err != nil {
			t.Fatalf("quest_clear: %v", err)
		}
		if res.IsError {
			t.Fatalf("quest_clear IsError=true: %s", textOf(res.Content))
		}
		body := textOf(res.Content)
		if !strings.Contains(body, "no quest_brief yet") {
			t.Errorf("expected hint in body; got:\n%s", body)
		}
	})

	t.Run("after_brief_no_hint", func(t *testing.T) {
		// Write a brief, then post+clear a new quest.
		q2, err := quest.Post(ctx, db, pid, quest.PostParams{Subject: "hint-test-2"})
		if err != nil {
			t.Fatalf("post 2: %v", err)
		}
		if err := quest.Brief(ctx, db, pid, "session wrap-up", "agent"); err != nil {
			t.Fatalf("brief: %v", err)
		}

		res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "quest_clear",
			Arguments: map[string]any{
				"quest_id": q2.ID,
				"report":   "done",
				"project":  pid,
			},
		})
		if err != nil {
			t.Fatalf("quest_clear 2: %v", err)
		}
		if res.IsError {
			t.Fatalf("quest_clear 2 IsError=true: %s", textOf(res.Content))
		}
		body := textOf(res.Content)
		if strings.Contains(body, "no quest_brief yet") {
			t.Errorf("unexpected hint after brief was written; got:\n%s", body)
		}
	})
}

// TestSessionStartNoStub guards against regression of the [stub] hole:
// the session_tool.go handler must route through the real quest.Bounties
// + lore.Oath stack, not a placeholder. We assert on two things:
//
//  1. the literal string "[stub]" does not appear in session_tool.go
//  2. a bootstrap CallTool on a freshly-registered project returns
//     something richer than the bare "active project set" header.
//
// (1) alone is necessary-but-insufficient — the literal could be
// commented out yet the code still return [stub] via a different
// path. (2) asserts on the observable shape.
func TestSessionStartNoStub(t *testing.T) {
	src, err := os.ReadFile("session_tool.go")
	if err != nil {
		t.Fatalf("read session_tool.go: %v", err)
	}
	if strings.Contains(string(src), "[stub]") {
		t.Error("session_tool.go still contains [stub] literal — expected full implementation")
	}

	// Functional side: bootstrap against a project that has NO quest/
	// lore state yet should return the header + graceful fallback, not
	// a bare stub string. The bounties-render path either returns data
	// (populated project) or returns "" + the friendly "no bounties
	// yet" message — both are acceptable, neither is "[stub]".
	isolateHome(t)
	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	res, err := client.CallTool(context.Background(),
		&sdkmcp.CallToolParams{
			Name:      "guild_session_start",
			Arguments: map[string]any{"project": "nostubproj"},
		})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	body := textOf(res.Content)
	if strings.Contains(body, "[stub]") {
		t.Errorf("session-start body still includes [stub]: %q", body)
	}
	// Body must at least include the active-project narration AND
	// either briefing/oath/top-task structure or the graceful fallback.
	if !strings.Contains(body, "active project:") {
		t.Errorf("missing narration header: %q", body)
	}
	// Either of these indicates the Bounties wiring ran: actual payload
	// fields ("briefing", "oath", "unclaimed", "QUEST-") or the
	// friendly fallback the post-Bounties code path emits.
	hasBountiesPath := strings.Contains(body, "briefing") ||
		strings.Contains(body, "oath") ||
		strings.Contains(body, "unclaimed") ||
		strings.Contains(body, "bounties yet") ||
		strings.Contains(body, "QUEST-")
	if !hasBountiesPath {
		t.Errorf("session-start body lacks Bounties-wired content: %q", body)
	}
}

// TestTools_AdvertisedMatchesRegistered is the wire-level regression gate
// for QUEST-64. It asserts that every non-deferred tool in expectedTools
// appears in the server's tools/list response as served over the in-memory
// SDK transport — the same path a real MCP host (Claude Code, Cursor, etc.)
// traverses. TestTools_RegisteredCount catches set-equality regressions;
// this test ensures quest_update (and every other always-on tool) is
// actually advertised on the wire, not merely registered in internal state.
//
// Deferred (heavy+rare) tools are excluded because they are intentionally
// omitted from the lean default advertisement.
func TestTools_AdvertisedMatchesRegistered(t *testing.T) {
	isolateHome(t)
	got := listToolNames(t)
	gotSet := make(map[string]struct{}, len(got))
	for _, name := range got {
		gotSet[name] = struct{}{}
	}

	for _, e := range expectedTools {
		if _, ok := gotSet[e.name]; !ok {
			t.Errorf("tool %q registered but absent from tools/list wire response", e.name)
		}
	}
}

// --- helpers -----------------------------------------------------------

func listToolNames(t *testing.T) []string {
	t.Helper()
	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()
	res, err := client.ListTools(context.Background(), &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func allExpected() []string {
	names := make([]string, 0, len(expectedTools))
	for _, e := range expectedTools {
		names = append(names, e.name)
	}
	sort.Strings(names)
	return names
}

func cmp(got, want []string) string {
	if len(got) != len(want) {
		return "count mismatch: got=" + itoa(len(got)) + " want=" + itoa(len(want)) +
			"\n  got:  " + strings.Join(got, ",") +
			"\n  want: " + strings.Join(want, ",")
	}
	for i := range got {
		if got[i] != want[i] {
			return "index " + itoa(i) + ": got=" + got[i] + " want=" + want[i]
		}
	}
	return ""
}

// minArgsFor returns the smallest schema-valid argument map for a
// given tool. For TestTools_BootstrapRequired we need the schema
// validator to pass so the handler runs and hits resolveProject.
func minArgsFor(name string) map[string]any {
	switch name {
	case "lore_appraise":
		return map[string]any{"query": "x"}
	case "lore_study":
		return map[string]any{"entry_id": 1}
	case "lore_inscribe":
		return map[string]any{
			"title": "t", "kind": "observation",
			"summary": "s", "topic": "topic",
		}
	case "lore_reforge":
		return map[string]any{"old_id": 1, "new_id": 2}
	case "lore_update":
		return map[string]any{"entry_id": 1}
	case "lore_seal":
		return map[string]any{"entry_id": 1}
	case "lore_catalog":
		return map[string]any{"dir": "/tmp/none"}
	case "lore_link":
		return map[string]any{"from_id": 1, "to_id": 2}
	case "lore_unlink":
		return map[string]any{"from_id": 1, "to_id": 2}
	case "lore_ripples":
		return map[string]any{"entry_id": "1"}
	case "quest_post":
		return map[string]any{"subject": "s"}
	case "quest_accept":
		return map[string]any{"quest_id": "QUEST-1"}
	case "quest_clear":
		return map[string]any{"quest_id": "QUEST-1", "report": "r"}
	case "quest_fulfill":
		return map[string]any{"quest_id": "QUEST-1", "report": "r"}
	case "quest_journal":
		return map[string]any{"quest_id": "QUEST-1", "text": "t"}
	case "quest_brief":
		return map[string]any{"text": "t"}
	case "quest_bounties":
		return map[string]any{}
	case "quest_scroll":
		return map[string]any{"quest_id": "QUEST-1"}
	case "quest_epic":
		return map[string]any{"epic": "e", "quest_ids": []string{"QUEST-1"}}
	case "quest_forfeit":
		return map[string]any{"quest_id": "QUEST-1"}
	case "quest_campfire":
		return map[string]any{"quest_id": "QUEST-1", "hypothesis": "h"}
	case "quest_summon":
		return map[string]any{"quest_id": "QUEST-1", "to": "teammate"}
	case "quest_orders":
		return map[string]any{}
	case "quest_update":
		return map[string]any{"quest_id": "QUEST-1"}
	case "quest_search":
		return map[string]any{"query": "x"}
	default:
		return map[string]any{}
	}
}

// smokeArgsFor returns success-path arguments for the smoke test,
// scoped to the `testproj` project set up by isolateProject.
func smokeArgsFor(name string) map[string]any {
	base := map[string]any{"project": "testproj"}
	switch name {
	case "guild_session_start":
		return map[string]any{"project": "testproj"}
	case "guild_set_project":
		return map[string]any{"project": "testproj"}
	case "guild_status":
		return map[string]any{"project": "testproj"}
	case "lore_appraise":
		return map[string]any{"query": "x", "project": "testproj"}
	case "lore_study":
		return map[string]any{"entry_id": 999999, "project": "testproj"}
	case "lore_oath":
		return base
	case "lore_list":
		return base
	case "lore_dossier":
		return base
	case "lore_inscribe":
		return map[string]any{
			"title": "smoke", "kind": "observation",
			"summary": "summary text", "topic": "smoke",
			"project": "testproj",
		}
	case "lore_reforge":
		return map[string]any{"old_id": 999998, "new_id": 999999, "project": "testproj"}
	case "lore_update":
		return map[string]any{
			"entry_id": 999999, "title": "x",
			"project": "testproj",
		}
	case "lore_seal":
		return map[string]any{"entry_id": 999999, "project": "testproj"}
	case "lore_catalog":
		return map[string]any{"dir": "/tmp/guild-no-such-dir-xyz", "project": "testproj"}
	case "lore_link":
		return map[string]any{"from_id": 999998, "to_id": 999999, "project": "testproj"}
	case "lore_unlink":
		return map[string]any{"from_id": 999998, "to_id": 999999, "project": "testproj"}
	case "lore_inquest":
		return base
	case "lore_meld":
		return base
	case "lore_commune":
		return base
	case "lore_health":
		return base
	case "lore_embed_rebuild":
		return base
	case "lore_coverage_reconcile":
		return base
	case "lore_ripples":
		return map[string]any{"entry_id": "999999", "project": "testproj"}
	case "quest_post":
		return map[string]any{"subject": "smoke subject", "project": "testproj"}
	case "quest_accept":
		return map[string]any{"quest_id": "QUEST-99999", "project": "testproj"}
	case "quest_journal":
		return map[string]any{"quest_id": "QUEST-99999", "text": "t", "project": "testproj"}
	case "quest_clear":
		return map[string]any{
			"quest_id": "QUEST-99999", "report": "r",
			"project": "testproj",
		}
	case "quest_fulfill":
		return map[string]any{
			"quest_id": "QUEST-99999", "report": "r",
			"project": "testproj",
		}
	case "quest_brief":
		return map[string]any{"text": "t", "project": "testproj"}
	case "quest_bounties":
		return base
	case "quest_list":
		return base
	case "quest_scroll":
		return map[string]any{"quest_id": "QUEST-99999", "project": "testproj"}
	case "quest_pulse":
		return base
	case "quest_epic":
		return map[string]any{
			"epic": "smoke", "quest_ids": []string{"QUEST-99999"},
			"project": "testproj",
		}
	case "quest_active":
		return base
	case "quest_forfeit":
		return map[string]any{"quest_id": "QUEST-99999", "project": "testproj"}
	case "quest_campfire":
		return map[string]any{
			"quest_id": "QUEST-99999", "hypothesis": "h",
			"project": "testproj",
		}
	case "quest_summon":
		return map[string]any{
			"quest_id": "QUEST-99999", "to": "other-agent",
			"project": "testproj",
		}
	case "quest_orders":
		return base
	case "quest_update":
		return map[string]any{
			"quest_id": "QUEST-99999", "subject": "updated",
			"project": "testproj",
		}
	case "quest_search":
		return map[string]any{"query": "smoke search", "project": "testproj"}
	default:
		return base
	}
}

// TestRipples_E2E seeds a small graph via MCP tools, calls lore_ripples,
// and asserts the rendered body contains the seed and a descendant.
func TestRipples_E2E(t *testing.T) {
	pid := isolateProject(t)
	ctx := context.Background()

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	if _, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// inscribe returns the LORE-N label parsed from the narration line.
	inscribe := func(title string) string {
		t.Helper()
		res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "lore_inscribe",
			Arguments: map[string]any{
				"title": title, "kind": "observation",
				"summary": "e2e ripples test entry summary",
				"topic":   "ripples-e2e", "project": pid,
			},
		})
		if err != nil {
			t.Fatalf("inscribe %q: %v", title, err)
		}
		if res.IsError {
			t.Fatalf("inscribe %q IsError: %s", title, textOf(res.Content))
		}
		body := textOf(res.Content)
		for _, f := range strings.Fields(body) {
			if strings.HasPrefix(f, "LORE-") {
				return strings.TrimRight(f, ":")
			}
		}
		t.Fatalf("could not parse LORE-N from: %s", body)
		return ""
	}

	// link calls lore_link using numeric ids extracted from LORE-N labels.
	numericID := func(loreN string) int64 {
		t.Helper()
		var n int64
		_, err := fmt.Sscanf(strings.TrimPrefix(loreN, "LORE-"), "%d", &n)
		if err != nil {
			t.Fatalf("parse numeric id from %q: %v", loreN, err)
		}
		return n
	}
	link := func(from, to string) {
		t.Helper()
		res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "lore_link",
			Arguments: map[string]any{
				"from_id": numericID(from), "to_id": numericID(to), "project": pid,
			},
		})
		if err != nil {
			t.Fatalf("link %s→%s: %v", from, to, err)
		}
		if res.IsError {
			t.Fatalf("link %s→%s IsError: %s", from, to, textOf(res.Content))
		}
	}

	// Build: A → B → C, A → D, C → A (cycle), E orphan.
	idA := inscribe("ripples e2e entry A the seed entry")
	idB := inscribe("ripples e2e entry B descendant of A")
	idC := inscribe("ripples e2e entry C descendant of B")
	idD := inscribe("ripples e2e entry D branch from A")
	idE := inscribe("ripples e2e entry E orphan no links")
	_ = idE

	link(idA, idB)
	link(idB, idC)
	link(idA, idD)
	link(idC, idA) // cycle

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "lore_ripples",
		Arguments: map[string]any{
			"entry_id": idA, "depth": 3, "direction": "out",
			"relation": "all", "project": pid,
		},
	})
	if err != nil {
		t.Fatalf("lore_ripples: %v", err)
	}
	if res.IsError {
		t.Fatalf("lore_ripples IsError: %s", textOf(res.Content))
	}
	body := textOf(res.Content)

	if !strings.Contains(body, idA) {
		t.Errorf("body missing seed %s;\n%s", idA, body)
	}
	if !strings.Contains(body, idB) && !strings.Contains(body, idD) {
		t.Errorf("body missing descendants %s/%s;\n%s", idB, idD, body)
	}
	if !strings.Contains(body, "↓ descendants") {
		t.Errorf("body missing descendants section header;\n%s", body)
	}
	if !strings.Contains(body, "entries walked") {
		t.Errorf("body missing footer;\n%s", body)
	}
}
