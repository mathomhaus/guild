package lore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

func TestAppraise_EmptyQuery(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := Appraise(ctx, db, AppraiseParams{Query: "   "})
	if err == nil {
		t.Fatalf("expected ErrEmptyQuery")
	}
}

func TestAppraise_CrossProjectDefault(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	// 3 entries in 3 different projects, each with a shared keyword.
	seedCorpus(t, ctx, db, []fixtureEntry{
		{"pA", "research", "shared keyword in project A", "summary A", "t"},
		{"pB", "research", "shared keyword in project B", "summary B", "t"},
		{"pC", "research", "shared keyword in project C", "summary C", "t"},
	})

	// all_projects=true should return all 3
	out, err := Appraise(ctx, db, AppraiseParams{
		Query:       "shared",
		AllProjects: true,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("appraise all_projects: %v", err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("all_projects: got %d results, want 3", len(out.Results))
	}
	if len(out.ProjectCounts) != 3 {
		t.Fatalf("project counts = %v, want 3 distinct projects", out.ProjectCounts)
	}

	// project-scoped should return only 1
	out, err = Appraise(ctx, db, AppraiseParams{
		Query:   "shared",
		Project: "pB",
		Now:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("appraise project-scoped: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("project=pB: got %d results, want 1", len(out.Results))
	}
	if out.Results[0].Entry.ProjectID != "pB" {
		t.Fatalf("project=pB: got project %q", out.Results[0].Entry.ProjectID)
	}
}

func TestAppraise_SlugDetectMissHint(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	// Seed something so the DB isn't empty, but nothing matching "QUEST-42".
	seedCorpus(t, ctx, db, []fixtureEntry{
		{"pA", "decision", "a real title", "a real summary", "t"},
	})

	out, err := Appraise(ctx, db, AppraiseParams{
		Query:       "QUEST-42",
		AllProjects: true,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	if len(out.Results) != 0 {
		t.Fatalf("expected zero results for QUEST-42, got %d", len(out.Results))
	}
	if !strings.Contains(out.MissHint, "quest") {
		t.Fatalf("expected slug hint referencing quest, got %q", out.MissHint)
	}
}

func TestAppraise_SlugDetectHyphenated(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()
	seedCorpus(t, ctx, db, []fixtureEntry{
		{"pA", "decision", "a real title", "a real summary", "t"},
	})

	out, err := Appraise(ctx, db, AppraiseParams{
		Query:       "cross-project-dedup",
		AllProjects: true,
		Now:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	if len(out.Results) != 0 {
		t.Fatalf("expected zero results, got %d", len(out.Results))
	}
	if !strings.Contains(out.MissHint, "slug-like") {
		t.Fatalf("expected slug-like hint, got %q", out.MissHint)
	}
}

func TestAppraise_SinceFiltersRecent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	// Insert an old entry and a fresh entry; since=7d should skip the old.
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','research','old freshness marker','old summary','current',?,?)`,
		now.AddDate(0, 0, -60).Format(time.RFC3339),
		now.AddDate(0, 0, -60).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','research','fresh freshness marker','fresh summary','current',?,?)`,
		now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	out, err := Appraise(ctx, db, AppraiseParams{
		Query:       "freshness marker",
		Since:       7 * 24 * time.Hour,
		AllProjects: true,
		Now:         now,
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("since=7d: got %d results, want 1", len(out.Results))
	}
	if !strings.Contains(out.Results[0].Entry.Title, "fresh") {
		t.Fatalf("since=7d returned wrong entry: %q", out.Results[0].Entry.Title)
	}
}

// TestAppraise_SinceUsesInjectedNow guards the contract that Since-filtering
// and scoring share the same reference clock. Before the fix, buildWhereClause
// called time.Now() independently, so an injected AppraiseParams.Now in the
// past caused the wall-clock cutoff to exclude entries that were inside the
// injected window — making deterministic tests and historical replays silently
// drop valid results.
func TestAppraise_SinceUsesInjectedNow(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	// Pin "now" to 30 days in the past relative to the real wall clock.
	// An entry created 1 hour before that pinned instant sits inside a
	// 7-day Since window measured from pinnedNow, but outside a 7-day
	// window measured from the real wall clock — so the test fails
	// without the fix.
	pinnedNow := time.Now().UTC().AddDate(0, 0, -30)
	entryTime := pinnedNow.Add(-1 * time.Hour)

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','research','injected clock sentinel','sentinel summary','current',?,?)`,
		entryTime.Format(time.RFC3339), entryTime.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	out, err := Appraise(ctx, db, AppraiseParams{
		Query:       "injected clock sentinel",
		Since:       7 * 24 * time.Hour,
		AllProjects: true,
		Now:         pinnedNow,
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("injected Now: got %d results, want 1 (entry inside Since window relative to pinnedNow)", len(out.Results))
	}
	if !strings.Contains(out.Results[0].Entry.Title, "sentinel") {
		t.Fatalf("injected Now: wrong entry returned: %q", out.Results[0].Entry.Title)
	}
}

func TestAppraise_AllFlagIncludesArchived(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','research','archived entry widget','summary','archived',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// Default: archived should be hidden.
	outDefault, err := Appraise(ctx, db, AppraiseParams{
		Query:       "archived widget",
		AllProjects: true,
	})
	if err != nil {
		t.Fatalf("appraise default: %v", err)
	}
	if len(outDefault.Results) != 0 {
		t.Fatalf("default: expected 0 archived results, got %d", len(outDefault.Results))
	}

	// With IncludeAll, archived should surface.
	outAll, err := Appraise(ctx, db, AppraiseParams{
		Query:       "archived widget",
		AllProjects: true,
		IncludeAll:  true,
	})
	if err != nil {
		t.Fatalf("appraise IncludeAll: %v", err)
	}
	if len(outAll.Results) != 1 {
		t.Fatalf("IncludeAll: got %d, want 1", len(outAll.Results))
	}
}

func TestAppraise_BumpsAccessCounters(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	ids := seedCorpus(t, ctx, db, []fixtureEntry{
		{"p", "research", "counter test entry", "stuff", "t"},
	})

	_, err := Appraise(ctx, db, AppraiseParams{
		Query:       "counter test",
		AllProjects: true,
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	row := db.QueryRowContext(ctx,
		`SELECT access_count FROM entries WHERE id = ?`, ids[0])
	var count int
	if err := row.Scan(&count); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 1 {
		t.Fatalf("access_count = %d, want 1", count)
	}
}

func TestAppraise_PerQueryWeightOverride(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	// Two entries: old + highly relevant; fresh + weakly relevant.
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -180).Format(time.RFC3339)
	fresh := now.Format(time.RFC3339)

	res, err := db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','research','deep match on distinctive rarewords alpha beta gamma','stuff','current',?,?)`,
		old, old)
	if err != nil {
		t.Fatal(err)
	}
	oldID, _ := res.LastInsertId()

	res2, err := db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','research','fresh entry only mentions rarewords once','stuff','current',?,?)`,
		fresh, fresh)
	if err != nil {
		t.Fatal(err)
	}
	freshID, _ := res2.LastInsertId()

	// Default: BM25 weight dominates; old distinctive match wins.
	outDefault, err := Appraise(ctx, db, AppraiseParams{
		Query:       "distinctive rarewords alpha beta gamma",
		AllProjects: true,
		Now:         now,
	})
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if outDefault.Results[0].Entry.ID != oldID {
		t.Fatalf("default weights: got top ID %d, want old %d", outDefault.Results[0].Entry.ID, oldID)
	}

	// Recency-heavy weights should flip the order.
	recencyHeavy := DefaultScoring()
	recencyHeavy.WFTS = 0.05
	recencyHeavy.WRecency = 0.95
	// Disable title boosts so only BM25/recency decide.
	recencyHeavy.TitleMatchBoost = 0
	recencyHeavy.TitleTokenBoost = 0
	outRecency, err := Appraise(ctx, db, AppraiseParams{
		Query:       "rarewords",
		AllProjects: true,
		Scoring:     recencyHeavy,
		Now:         now,
	})
	if err != nil {
		t.Fatalf("recency: %v", err)
	}
	if outRecency.Results[0].Entry.ID != freshID {
		t.Fatalf("recency-heavy weights: got top ID %d, want fresh %d (%v)",
			outRecency.Results[0].Entry.ID, freshID, outRecency.Results)
	}
}

// TestAppraise_CrossProjectNoCap verifies LORE-374 / LORE-371: when the
// RRF path is active (embedder enabled, coverage satisfied) and one
// project genuinely dominates for a query, all top-N results come from
// that project rather than one-per-project samples.
//
// The old design ran per-project BM25+vector pairs and merged them via
// FuseMany. Because every project's rank-1 entry received an equal RRF
// score (1/(k+1)), the final top-N distributed roughly one slot per
// project regardless of how many matching entries a single project had.
// The fix replaces the per-project loop with a single global BM25 query
// and a single global vector TopK, fused together, so the scores reflect
// actual global relevance.
func TestAppraise_CrossProjectNoCap(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	// "dominant" has 5 entries all matching "frobnitz primary".
	// Four noise projects each have 1 entry also matching "frobnitz",
	// so they compete in BM25. Before the fix, each of the 5 projects
	// would claim one slot in the top-5, leaving dominant with at most 1.
	seedCorpus(t, ctx, db, []fixtureEntry{
		{"dominant", "research", "frobnitz protocol alpha primary", "s", "frobnitz"},
		{"dominant", "research", "frobnitz protocol beta primary", "s", "frobnitz"},
		{"dominant", "research", "frobnitz protocol gamma primary", "s", "frobnitz"},
		{"dominant", "research", "frobnitz protocol delta primary", "s", "frobnitz"},
		{"dominant", "research", "frobnitz protocol epsilon primary", "s", "frobnitz"},
		{"noise1", "research", "frobnitz noise one entry", "s", "frobnitz"},
		{"noise2", "research", "frobnitz noise two entry", "s", "frobnitz"},
		{"noise3", "research", "frobnitz noise three entry", "s", "frobnitz"},
		{"noise4", "research", "frobnitz noise four entry", "s", "frobnitz"},
	})

	// Activate the RRF cross-project path: embedder_state=enabled and
	// coverage 1/1=100%. DeterministicEmbedder returns non-nil vectors
	// so Embed() succeeds. Index=nil: no vector arm, so ranking is
	// BM25-only via RRF, which is sufficient to assert global domination.
	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"vector_coverage_num", "1"},
		{"vector_coverage_den", "1"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	embedDeps := &EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "test-model",
	}

	out, err := Appraise(ctx, db, AppraiseParams{
		Query:       "frobnitz primary",
		AllProjects: true,
		Limit:       5,
		Now:         time.Now().UTC(),
		Embed:       embedDeps,
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	if len(out.Results) != 5 {
		t.Fatalf("want 5 results, got %d", len(out.Results))
	}
	for i, r := range out.Results {
		if r.Entry.ProjectID != "dominant" {
			t.Errorf("result[%d]: project=%q, want dominant (one-per-project sampling was not removed)", i, r.Entry.ProjectID)
		}
	}
}

func TestFTSQuery_Sanitization(t *testing.T) {
	// FTS5 reserved chars must be stripped, not passed through.
	cases := []struct {
		in   string
		want string
	}{
		{"hello world", "hello* OR world*"},
		{`"quoted"`, "quoted*"},
		{"minus-sign", "minus* OR sign*"},
		{"a", ""},          // too short, dropped
		{"", ""},           // empty query → empty output
		{"!@#$%^&*()", ""}, // only punctuation → empty output
	}
	for _, tc := range cases {
		got := ftsQuery(tc.in)
		if got != tc.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1m", 30 * 24 * time.Hour},
	}
	for _, tc := range cases {
		got, err := ParseSince(tc.in)
		if err != nil {
			t.Errorf("ParseSince(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSince(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := ParseSince("5y"); err == nil {
		t.Errorf("ParseSince(5y) should fail")
	}
	if _, err := ParseSince(""); err == nil {
		t.Errorf("ParseSince('') should fail")
	}
}

func TestSlugHint(t *testing.T) {
	cases := []struct {
		in       string
		wantHint bool
	}{
		{"QUEST-42", true},
		{"cross-project-dedup", true},
		{"a multi word query", false},
		{"singleword", false},
		{"", false},
	}
	for _, tc := range cases {
		got := slugHint(tc.in)
		hasHint := got != ""
		if hasHint != tc.wantHint {
			t.Errorf("slugHint(%q) = %q, wantHint=%v", tc.in, got, tc.wantHint)
		}
	}
}
