package mcp

import (
	"context"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/quest"
)

// TestHints_E2E_InscribeLooksLikeQuest is the manual-smoke path converted
// to Go so CI catches regressions: an MCP client calls lore_inscribe with
// a TODO-laden title; the engine should fire the inscribe-looks-like-quest
// rule and the response body should carry the 💡 hint line.
func TestHints_E2E_InscribeLooksLikeQuest(t *testing.T) {
	pid := isolateProject(t)
	ctx := context.Background()

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	// Bootstrap so no-session-start doesn't steal the budget.
	if _, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "lore_inscribe",
		Arguments: map[string]any{
			"title":   "TODO wire the foo module",
			"summary": "we need to connect foo to bar before shipping",
			"kind":    "observation",
			"topic":   "hints-smoke",
			"project": pid,
		},
	})
	if err != nil {
		t.Fatalf("lore_inscribe: %v", err)
	}
	if res.IsError {
		t.Fatalf("lore_inscribe IsError=true: %s", textOf(res.Content))
	}
	body := textOf(res.Content)
	if !strings.Contains(body, "💡") {
		t.Errorf("expected 💡 hint in body; got:\n%s", body)
	}
	if !strings.Contains(body, "quest_post") {
		t.Errorf("expected hint to mention quest_post; got:\n%s", body)
	}
}

// TestHints_E2E_NoSessionStart_SuppressedAfterBootstrap is the QUEST-72
// regression: guild_session_start runs, then lore_appraise with a query
// that would otherwise attract no-session-start must NOT emit that hint.
// The historical bug was that register.go built separate hint engines for
// the lore-side and quest-side Deps, so hintsRecordBootstrap updated only
// the latest engine while the earlier-bound lore tools kept routing
// through the stale engine.
func TestHints_E2E_NoSessionStart_SuppressedAfterBootstrap(t *testing.T) {
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

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "lore_appraise",
		Arguments: map[string]any{
			"query":   "day1",
			"project": pid,
		},
	})
	if err != nil {
		t.Fatalf("lore_appraise: %v", err)
	}
	if res.IsError {
		t.Fatalf("lore_appraise IsError=true: %s", textOf(res.Content))
	}
	body := textOf(res.Content)
	if strings.Contains(body, "no guild_session_start yet this session") {
		t.Errorf("no-session-start fired after bootstrap; body:\n%s", body)
	}
}

// TestHints_E2E_SlugQuery_OnlyOnZeroResult is the QUEST-73 regression:
// the slug-query rule must fire on a slug-shaped query that returned no
// rows, and must NOT fire when the same-shaped query returned useful
// hits. Pre-fix behavior fired on every slug-shaped query regardless of
// result count (the QUEST-58 migration dropped lore.slugHint's
// len(rows)==0 gate).
func TestHints_E2E_SlugQuery_OnlyOnZeroResult(t *testing.T) {
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

	// Seed one lore entry whose title matches the upcoming slug query so
	// the search returns at least one hit. Use kind=observation so no
	// other hint (principle-too-long, inscribe-looks-like-quest) tries to
	// win the budget slot.
	if _, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "lore_inscribe",
		Arguments: map[string]any{
			"title":   "seeded slug target entry",
			"summary": "plain summary so todo phrases do not fire.",
			"kind":    "observation",
			"topic":   "slug-seed",
			"project": pid,
		},
	}); err != nil {
		t.Fatalf("seed inscribe: %v", err)
	}

	// Case 1: slug-shaped query with a hit — slug-query must NOT fire.
	hitRes, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "lore_appraise",
		Arguments: map[string]any{
			"query":   "seeded-slug-target",
			"project": pid,
		},
	})
	if err != nil {
		t.Fatalf("appraise hit: %v", err)
	}
	hitBody := textOf(hitRes.Content)
	if strings.Contains(hitBody, "looks slug-like") {
		t.Errorf("slug-query fired on successful search; body:\n%s", hitBody)
	}

	// Case 2: slug-shaped query with zero hits — slug-query MUST fire.
	missRes, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "lore_appraise",
		Arguments: map[string]any{
			"query":   "QUEST-9999999",
			"project": pid,
		},
	})
	if err != nil {
		t.Fatalf("appraise miss: %v", err)
	}
	missBody := textOf(missRes.Content)
	if !strings.Contains(missBody, "looks slug-like") {
		t.Errorf("slug-query did not fire on zero-result slug query; body:\n%s", missBody)
	}
}

// TestHints_E2E_NoBrief24h_BothVerbsFire is the QUEST-137 regression test:
// the no-brief-24h rule must fire on BOTH quest_fulfill (canonical) AND
// quest_clear (backward-compat alias). Each sub-test spins up an isolated
// server so the singleton engine's session state is fresh per run.
func TestHints_E2E_NoBrief24h_BothVerbsFire(t *testing.T) {
	longReport := "commit abc123, files: x.go y.go; follow-ups: run integration tests and verify deploy pipeline stays green"

	for _, toolName := range []string{"quest_fulfill", "quest_clear"} {
		toolName := toolName // capture range variable for sub-test closure
		t.Run(toolName, func(t *testing.T) {
			pid := isolateProject(t)
			ctx := context.Background()

			db, err := openQuestDB(ctx)
			if err != nil {
				t.Fatalf("open quest db: %v", err)
			}
			defer func() { _ = db.Close() }()

			q, err := quest.Post(ctx, db, pid, quest.PostParams{Subject: "brief-regression-" + toolName})
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

			// Use the tool under test — either quest_fulfill or quest_clear.
			// A long report keeps clear-without-report-detail from winning the
			// budget slot over no-brief-24h.
			res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
				Name: toolName,
				Arguments: map[string]any{
					"quest_id": q.ID,
					"report":   longReport,
					"project":  pid,
				},
			})
			if err != nil {
				t.Fatalf("%s: %v", toolName, err)
			}
			if res.IsError {
				t.Fatalf("%s IsError=true: %s", toolName, textOf(res.Content))
			}
			body := textOf(res.Content)
			if !strings.Contains(body, "no quest_brief yet") {
				t.Errorf("%s: expected no-brief-24h hint in body; got:\n%s", toolName, body)
			}
			if !strings.Contains(body, "💡") {
				t.Errorf("%s: expected 💡 emoji in body; got:\n%s", toolName, body)
			}
		})
	}
}

// TestHints_E2E_ClearWithoutBrief fires no-brief-24h through a full MCP
// round-trip — the canonical migration target of BriefHint.
func TestHints_E2E_ClearWithoutBrief(t *testing.T) {
	pid := isolateProject(t)
	ctx := context.Background()

	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()

	q, err := quest.Post(ctx, db, pid, quest.PostParams{Subject: "smoke-brief"})
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

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "quest_clear",
		Arguments: map[string]any{
			"quest_id": q.ID,
			// A long enough report so clear-without-report-detail doesn't
			// win the budget slot over no-brief-24h.
			"report":  "commit abc123, files: x.go y.go; follow-ups: run integration tests and verify deploy pipeline stays green",
			"project": pid,
		},
	})
	if err != nil {
		t.Fatalf("quest_clear: %v", err)
	}
	if res.IsError {
		t.Fatalf("quest_clear IsError=true: %s", textOf(res.Content))
	}
	body := textOf(res.Content)
	// The no-brief-24h rule's template starts with "no quest_brief yet".
	if !strings.Contains(body, "no quest_brief yet") {
		t.Errorf("expected no-brief-24h hint in body; got:\n%s", body)
	}
	if !strings.Contains(body, "💡") {
		t.Errorf("expected 💡 emoji; got:\n%s", body)
	}
}
