package hints

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/mathomhaus/guild/internal/storage"
)

// testingTB is the subset of *testing.T/*testing.B the helpers consume.
// Lets engine_bench_test.go reuse newTestStore without duplication.
type testingTB interface {
	Helper()
	TempDir() string
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// newTestStore opens a fresh quest.db under t.TempDir with migrations
// applied, returning a Store ready for engine tests.
func newTestStore(t testingTB) (*Store, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hints.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.Migrate(ctx, db, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(db), db
}

// TestEngine_LoadRules_SeededFromMigration ensures the seeds from
// 001_init.up.sql and 002_thin_citation_hint.up.sql land the full
// 10-rule set.
func TestEngine_LoadRules_SeededFromMigration(t *testing.T) {
	store, _ := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	wantRules := []string{
		"inscribe-looks-like-quest",
		"no-session-start",
		"session-end-without-brief",
		"slug-query",
		"journal-outside-accepted",
		"no-brief-24h",
		"inscribe-without-appraise",
		"clear-without-report-detail",
		"inscribe-without-transfer-reasoning",
	}
	for _, id := range wantRules {
		if _, ok := eng.rules[id]; !ok {
			t.Errorf("missing rule %s", id)
		}
	}
}

// TestEngine_Evaluate_InscribeLooksLikeQuest_Fires end-to-end.
func TestEngine_Evaluate_InscribeLooksLikeQuest_Fires(t *testing.T) {
	store, _ := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	// Seed a session_start so no-session-start doesn't hijack the budget.
	eng.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})

	fire := eng.Evaluate(context.Background(), CallEvent{
		Tool: "lore_inscribe",
		Args: map[string]any{
			"title":   "TODO migrate WAL config",
			"summary": "need to update the config",
		},
	})
	if fire.Empty() {
		t.Fatal("expected a fire; got empty")
	}
	if fire.RuleID != "inscribe-looks-like-quest" {
		t.Errorf("RuleID = %q, want inscribe-looks-like-quest", fire.RuleID)
	}
	if fire.Severity != SeverityHint {
		t.Errorf("severity = %s, want hint", fire.Severity)
	}
	if !strings.Contains(fire.Render(), "💡") {
		t.Errorf("Render missing 💡 emoji: %q", fire.Render())
	}
}

// TestEngine_Evaluate_Cooldown suppresses an immediate re-fire of the
// SAME rule. Different rules can still fire on the next call — the
// cooldown is per-rule, not global.
func TestEngine_Evaluate_Cooldown(t *testing.T) {
	store, _ := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	eng.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})
	// Pre-seed a lore_appraise so inscribe-without-appraise stays
	// suppressed and doesn't steal the budget slot on the re-fire.
	eng.Context().RecordEvent(CallEvent{Tool: "lore_appraise"})

	args := map[string]any{
		"title":   "TODO fix me",
		"summary": "something",
	}
	first := eng.Evaluate(context.Background(), CallEvent{
		Tool: "lore_inscribe", Args: args,
	})
	if first.Empty() || first.RuleID != "inscribe-looks-like-quest" {
		t.Fatalf("first call should fire inscribe-looks-like-quest; got %s", first.RuleID)
	}
	// Immediate re-fire with the same payload — the same rule is cooling.
	second := eng.Evaluate(context.Background(), CallEvent{
		Tool: "lore_inscribe", Args: args,
	})
	if second.RuleID == "inscribe-looks-like-quest" {
		t.Errorf("same-rule re-fire should be suppressed; got %s", second.RuleID)
	}
}

// TestEngine_Evaluate_FYICap caps ℹ️ fyi fires per session at 3.
func TestEngine_Evaluate_FYICap(t *testing.T) {
	store, _ := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	eng.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})

	for i := 0; i < 5; i++ {
		// Advance call count to clear 5-call cooldown.
		for j := 0; j < 6; j++ {
			eng.Context().RecordEvent(CallEvent{Tool: "spacer"})
		}
		eng.Evaluate(context.Background(), CallEvent{
			Tool: "lore_inscribe",
			Args: map[string]any{
				"kind":    "decision",
				"title":   "some decision title",
				"summary": "some short summary that does not trigger the principle bloat path",
			},
		})
	}
	if got := eng.Context().FYIFiresThisSession(); got > FYICapPerSession {
		t.Errorf("FYIFiresThisSession = %d, want <= %d", got, FYICapPerSession)
	}
}

// TestEngine_Evaluate_ContextualSuppression — don't fire when the
// suggested follow-through has been observed recently.
func TestEngine_Evaluate_ContextualSuppression(t *testing.T) {
	store, _ := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	eng.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})
	// Just called lore_appraise.
	eng.Context().RecordEvent(CallEvent{Tool: "lore_appraise"})

	// Triggering inscribe-without-appraise should be suppressed — the
	// FollowThrough (lore_appraise) was seen in the last 5.
	fire := eng.Evaluate(context.Background(), CallEvent{
		Tool: "lore_inscribe",
		Args: map[string]any{
			"title":   "research findings",
			"summary": "content",
			"kind":    "research",
		},
	})
	if fire.RuleID == "inscribe-without-appraise" {
		t.Errorf("inscribe-without-appraise should be suppressed by recent appraise")
	}
}

// TestEngine_Evaluate_EraAwareNoBrief demotes to fyi on Bash era.
func TestEngine_Evaluate_EraAwareNoBrief(t *testing.T) {
	store, _ := newTestStore(t)
	engMCP := NewEngine(store, "s-mcp", EraMCP)
	if err := engMCP.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	engMCP.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})
	mcpFire := engMCP.Evaluate(context.Background(), CallEvent{
		Tool: "quest_clear",
		Args: map[string]any{briefHintSessionKey: true, "report": strings.Repeat("w ", 30)},
	})
	if mcpFire.RuleID != "no-brief-24h" || mcpFire.Severity != SeverityHint {
		t.Errorf("MCP: rule=%s sev=%s; want no-brief-24h/hint", mcpFire.RuleID, mcpFire.Severity)
	}

	engBash := NewEngine(store, "s-bash", EraBash)
	if err := engBash.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	engBash.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})
	bashFire := engBash.Evaluate(context.Background(), CallEvent{
		Tool: "quest_clear",
		Args: map[string]any{briefHintSessionKey: true, "report": strings.Repeat("w ", 30)},
	})
	if bashFire.RuleID != "no-brief-24h" || bashFire.Severity != SeverityFYI {
		t.Errorf("Bash: rule=%s sev=%s; want no-brief-24h/fyi", bashFire.RuleID, bashFire.Severity)
	}
}

// TestEngine_Evaluate_BudgetOneFire ensures multi-rule fires collapse to
// one result and that the winner is stable across runs.
//
// Regression for QUEST-71: before the sort-by-rule-id fix, the budget
// selection iterated over e.rules (a map) in non-deterministic order.
// When no-session-start and inscribe-looks-like-quest both fired with the
// same hint severity, the winner changed run-to-run under the race
// detector's scheduling perturbation. The fix sorts rules by rule_id
// before evaluation so the first alphabetically wins among equal-rank
// ties — "inscribe-looks-like-quest" < "no-session-start".
func TestEngine_Evaluate_BudgetOneFire(t *testing.T) {
	store, _ := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	// Intentionally omit guild_session_start so no-session-start (hint)
	// is eligible alongside inscribe-looks-like-quest (hint) and
	// inscribe-without-appraise (fyi) for the same lore_inscribe call.
	// Both hint-rank rules compete for the single budget slot.
	fire := eng.Evaluate(context.Background(), CallEvent{
		Tool: "lore_inscribe",
		Args: map[string]any{
			"title":   "TODO fix me",
			"summary": "we need to fix this",
			"kind":    "observation",
		},
	})
	if fire.Empty() {
		t.Fatal("expected a fire")
	}
	if fire.Severity == SeverityFYI {
		t.Errorf("fyi should be out-ranked by a hint rule; got %s/%s",
			fire.RuleID, fire.Severity)
	}
	// The sort-by-rule-id tiebreak must produce the same winner every run:
	// "inscribe-looks-like-quest" < "no-session-start" alphabetically.
	// If this assertion is flaky under -race, the sort was lost.
	const wantWinner = "inscribe-looks-like-quest"
	if fire.RuleID != wantWinner {
		t.Errorf("budget tiebreak unstable: got %q, want %q — check sort in Evaluate",
			fire.RuleID, wantWinner)
	}
}

// TestEngine_FollowThrough_HitScored marks a fire as hit after the
// suggested action lands in the next event.
func TestEngine_FollowThrough_HitScored(t *testing.T) {
	store, db := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	eng.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})
	// Inscribe with TODO → fires inscribe-looks-like-quest.
	eng.Evaluate(context.Background(), CallEvent{
		Tool: "lore_inscribe",
		Args: map[string]any{"title": "TODO x", "summary": "need to y"},
	})
	// Subsequent quest_post satisfies FollowThrough.
	eng.Evaluate(context.Background(), CallEvent{Tool: "quest_post"})

	// Scan hint_fires: one row, followed_through=1.
	var fires, hits int
	err := db.QueryRowContext(context.Background(), `
		SELECT COUNT(*), COUNT(CASE WHEN followed_through=1 THEN 1 END)
		FROM hint_fires`).Scan(&fires, &hits)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if fires != 1 {
		t.Errorf("fires = %d, want 1", fires)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
}

// TestEngine_FollowThrough_WindowElapses scores a miss after the 10-call
// window closes with no matching event.
func TestEngine_FollowThrough_WindowElapses(t *testing.T) {
	store, db := newTestStore(t)
	eng := NewEngine(store, "s1", EraMCP)
	if err := eng.LoadRules(context.Background()); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	eng.Context().RecordEvent(CallEvent{Tool: "guild_session_start"})
	eng.Evaluate(context.Background(), CallEvent{
		Tool: "lore_inscribe",
		Args: map[string]any{"title": "TODO x", "summary": "need to y"},
	})
	// Advance > 10 events with no quest_post.
	for i := 0; i < FollowThroughWindow+2; i++ {
		eng.Evaluate(context.Background(), CallEvent{Tool: "noop"})
	}

	var fires, scored, hits int
	err := db.QueryRowContext(context.Background(), `
		SELECT COUNT(*),
		       COUNT(CASE WHEN followed_through IS NOT NULL THEN 1 END),
		       COUNT(CASE WHEN followed_through=1 THEN 1 END)
		FROM hint_fires`).Scan(&fires, &scored, &hits)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if fires < 1 {
		t.Errorf("fires = %d, want >= 1", fires)
	}
	if scored < 1 || hits != 0 {
		t.Errorf("scored=%d hits=%d; want scored>=1 hits=0", scored, hits)
	}
}
