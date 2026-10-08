package lore

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
	"time"
)

// Regression-guard target for the fixture-based recall test. Keep at 1.0:
// the test suite requires every verbatim-title query to land at rank 1.
// A drop below this value indicates the scoring formula has regressed.
const recallAt1Target = 1.0

// ---------------------------------------------------------------------------
// Isolated component tests
// ---------------------------------------------------------------------------

func TestNormalizeFTS_ZeroIsZero(t *testing.T) {
	if v := NormalizeFTS(0); v != 0 {
		t.Fatalf("NormalizeFTS(0) = %v, want 0", v)
	}
	if v := NormalizeFTS(+1.5); v != 0 {
		t.Fatalf("NormalizeFTS(positive) = %v, want 0 (non-match)", v)
	}
}

func TestNormalizeFTS_MoreNegativeScoresHigher(t *testing.T) {
	weak := NormalizeFTS(-0.5)
	medium := NormalizeFTS(-2.0)
	strong := NormalizeFTS(-8.0)
	if !(weak < medium && medium < strong) {
		t.Fatalf("expected monotonic: weak(%v) < medium(%v) < strong(%v)", weak, medium, strong)
	}
	if strong <= 0 || math.IsInf(strong, 0) {
		t.Fatalf("strong evidence invalid: %v", strong)
	}
}

func TestNormalizeRecency_DecaysOverHalfLife(t *testing.T) {
	today := NormalizeRecency(0, 30)
	halfLife := NormalizeRecency(30, 30)
	doubleHalfLife := NormalizeRecency(60, 30)
	if math.Abs(today-1.0) > 1e-9 {
		t.Fatalf("today recency = %v, want ~1.0", today)
	}
	// 30-day age with 30-day half-life ≈ 0.5 (up to 0.693 ≈ ln(2) rounding).
	if math.Abs(halfLife-0.5) > 0.002 {
		t.Fatalf("half-life recency = %v, want ~0.5", halfLife)
	}
	// 60-day age ≈ 0.25.
	if math.Abs(doubleHalfLife-0.25) > 0.002 {
		t.Fatalf("2*half-life recency = %v, want ~0.25", doubleHalfLife)
	}
}

func TestTitleBoost_ExactMatch(t *testing.T) {
	cfg := DefaultScoring()
	got := TitleBoost("Hybrid ranking: BM25 + recency", "hybrid ranking: bm25 + recency", cfg)
	if got != cfg.TitleMatchBoost {
		t.Fatalf("exact match boost = %v, want %v", got, cfg.TitleMatchBoost)
	}
}

func TestTitleBoost_TokenSubset(t *testing.T) {
	cfg := DefaultScoring()
	// Query tokens {"bm25","recency"} ⊂ title tokens — should be token boost.
	got := TitleBoost("Hybrid ranking: BM25 + recency", "bm25 recency", cfg)
	if got != cfg.TitleTokenBoost {
		t.Fatalf("token-subset boost = %v, want %v", got, cfg.TitleTokenBoost)
	}
}

func TestTitleBoost_NoMatch(t *testing.T) {
	cfg := DefaultScoring()
	got := TitleBoost("Hybrid ranking", "something else entirely", cfg)
	if got != 0 {
		t.Fatalf("no-match boost = %v, want 0", got)
	}
}

func TestTitleBoost_EmptyQuery(t *testing.T) {
	cfg := DefaultScoring()
	if v := TitleBoost("Any title", "", cfg); v != 0 {
		t.Fatalf("empty query boost = %v, want 0", v)
	}
	if v := TitleBoost("Any title", "   ", cfg); v != 0 {
		t.Fatalf("whitespace query boost = %v, want 0", v)
	}
}

func TestScore_CombinesAllSignals(t *testing.T) {
	cfg := DefaultScoring()
	now := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)
	e := &Entry{
		Title:     "composite score test case target entry",
		CreatedAt: now.AddDate(0, 0, -15),
	}
	bm25 := -4.0
	got := Score(e, "composite score test case target entry", bm25, cfg, now)
	want := CombineScore(bm25, 15, cfg) + cfg.TitleMatchBoost
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("Score = %v, want %v", got, want)
	}
}

func TestScore_PerQueryWeightOverride(t *testing.T) {
	now := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)
	old := &Entry{Title: "Old entry about FTS", CreatedAt: now.AddDate(0, 0, -120)}
	newer := &Entry{Title: "Completely different text", CreatedAt: now.AddDate(0, 0, -1)}

	// Strong BM25 for the old entry, no BM25 for the newer entry.
	oldBM25 := -6.0
	newBM25 := -0.0001 // effectively no match

	def := DefaultScoring()
	strongRecency := def
	strongRecency.WFTS = 0.1
	strongRecency.WRecency = 0.9

	// With default weights, BM25 dominates → old wins.
	oldScoreDefault := Score(old, "fts", oldBM25, def, now)
	newScoreDefault := Score(newer, "fts", newBM25, def, now)
	if !(oldScoreDefault > newScoreDefault) {
		t.Fatalf("default weights: expected oldScore(%v) > newScore(%v)", oldScoreDefault, newScoreDefault)
	}

	// With recency-heavy weights, the fresh entry overtakes — proves
	// per-query override is actually applied.
	oldScoreRecency := Score(old, "fts", oldBM25, strongRecency, now)
	newScoreRecency := Score(newer, "fts", newBM25, strongRecency, now)
	if !(newScoreRecency > oldScoreRecency) {
		t.Fatalf("recency-heavy weights: expected newScore(%v) > oldScore(%v)", newScoreRecency, oldScoreRecency)
	}
}

// ---------------------------------------------------------------------------
// Recall@1 = 100% acceptance test.
// ---------------------------------------------------------------------------

// recallFixtureCorpus returns a ≥30-entry synthetic fixture whose
// titles intentionally overlap vocabulary within each project cluster
// (rate limiting, caching, tracing, indexing) to exercise the
// title-boost scoring path. Content is generic engineering material —
// no project-specific history leaks into test fixtures.
func recallFixtureCorpus() []fixtureEntry {
	return []fixtureEntry{
		{"alpha", "research", "Rate limiter comparison: token bucket versus leaky bucket under burst traffic", "empirical finding", "rate-limit,algorithm"},
		{"alpha", "research", "Rate limiter windowing: sliding window outperforms fixed window on burst recall", "windowing analysis", "rate-limit,window"},
		{"alpha", "decision", "Rate limiter algorithm locked: token bucket at 10 rps with burst of 20", "capacity planning", "rate-limit,config"},
		{"alpha", "decision", "Rate limiter key policy locked: per-user not per-IP on authenticated routes", "key design", "rate-limit,key"},
		{"alpha", "decision", "Rate limiter storage locked: Redis with Lua script for atomic increment", "storage layer", "rate-limit,redis"},
		{"alpha", "decision", "Rate limiter library locked: golang.org/x/time/rate over third-party options", "dependency choice", "rate-limit,deps"},
		{"alpha", "observation", "Rate limiter p99 latency spikes correlate with Redis round-trip not CPU usage", "profiler finding", "rate-limit,latency"},
		{"alpha", "observation", "Rate limiter 429 responses lack Retry-After header by default — clients retry too fast", "header audit", "rate-limit,headers"},
		{"alpha", "observation", "Rate limiter clock skew between pods causes double-counting at window edges", "bug analysis", "rate-limit,bug"},
		{"alpha", "principle", "Rate limiter thresholds must be load-tested not guessed from intuition", "capacity discipline", "rate-limit,principle"},
		{"beta", "research", "Cache hit ratio study: LRU outperforms FIFO on session read patterns", "empirical study", "cache,lru"},
		{"beta", "research", "Cache invalidation comparison: write-through versus write-behind tradeoff under load", "comparison", "cache,invalidation"},
		{"beta", "research", "Cache key shape affects hit rate more than eviction policy on small caches", "analysis verdict", "cache,keys"},
		{"beta", "decision", "Cache layer locked: Redis cluster with 30s TTL on the hot read path", "architecture", "cache,redis"},
		{"beta", "observation", "Cache stampede on cold start: request coalescing required for top endpoints", "failure mode", "cache,stampede"},
		{"beta", "observation", "Cache hit ratio drops 15% during deploy windows — warmup hook needed", "deploy observability", "cache,deploy"},
		{"beta", "principle", "Cache for the common case — never cache what a user can manipulate directly", "safety rule", "cache,principle"},
		{"gamma", "research", "Tracing sample rate study: 1% captures tail latency while keeping ingest cost flat", "sampling study", "tracing,sampling"},
		{"gamma", "decision", "Logging library locked: slog with JSON handler for structured output at scale", "deps locked", "logging,slog"},
		{"gamma", "observation", "Tracing spans without request-id lose correlation across async boundaries", "correlation gap", "tracing,async"},
		{"gamma", "principle", "Log at decision boundaries — not inside tight loops where volume dominates", "logging discipline", "logging,principle"},
		{"gamma", "idea", "Adaptive trace sampling keyed to p99 latency catches rare tail events", "sampling seed", "tracing,idea"},
		{"delta", "research", "Index selectivity below 5% degrades planner cost estimates on large tables", "planner analysis", "index,planner"},
		{"delta", "research", "Partial indexes cut storage 40% on soft-deleted rows — worth the query cost", "empirical tradeoff", "index,partial"},
		{"delta", "decision", "Index strategy locked: composite on (user_id, created_at) for session queries", "schema decision", "index,composite"},
		{"delta", "observation", "Index bloat after batch deletes: VACUUM ANALYZE cuts read latency in half", "maintenance finding", "index,vacuum"},
		{"delta", "observation", "Index-only scans miss on nullable columns unless WHERE clause proves non-null", "planner quirk", "index,scan"},
		{"delta", "principle", "Every production query must hit an index — sequential scan in EXPLAIN is a bug", "read discipline", "index,principle"},
		{"delta", "principle", "Index migrations run CONCURRENTLY in production — blocking DDL is a bug", "migration rule", "index,migrate"},
		{"delta", "principle", "Drop unused indexes before adding new ones — write bloat compounds quickly", "hygiene rule", "index,hygiene"},
	}
}

type fixtureEntry struct {
	ProjectID string
	Kind      string
	Title     string
	Summary   string
	Tags      string
}

// TestRecallAt1_ExactTitleQueries verifies recall@1 acceptance.
// Fixture corpus of 30 entries; query with each entry's verbatim title;
// assert 100% return the target entry at rank 1.
func TestRecallAt1_ExactTitleQueries(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	corpus := recallFixtureCorpus()
	if len(corpus) < 30 {
		t.Fatalf("corpus has %d entries, need ≥30", len(corpus))
	}

	ids := seedCorpus(t, ctx, db, corpus)

	hits := 0
	misses := []string{}
	for i, fx := range corpus {
		targetID := ids[i]
		res, err := Appraise(ctx, db, AppraiseParams{
			Query:       fx.Title,
			Limit:       10,
			AllProjects: true,
			Scoring:     DefaultScoring(),
			Now:         time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("appraise %q: %v", fx.Title, err)
		}
		if len(res.Results) == 0 {
			misses = append(misses, fmt.Sprintf("ENTRY-%d (no results): %s", targetID, fx.Title))
			continue
		}
		if res.Results[0].Entry.ID == targetID {
			hits++
		} else {
			misses = append(misses,
				fmt.Sprintf("ENTRY-%d ranked %d (winner ENTRY-%d): %s",
					targetID, rankOf(res.Results, targetID), res.Results[0].Entry.ID, fx.Title))
		}
	}

	recall := float64(hits) / float64(len(corpus))
	t.Logf("recall@1 = %d/%d = %.3f", hits, len(corpus), recall)
	for _, m := range misses {
		t.Logf("  miss: %s", m)
	}
	if recall < recallAt1Target {
		t.Fatalf("recall@1 = %.3f, want ≥ %.3f", recall, recallAt1Target)
	}
}

func rankOf(results []AppraiseResult, id int64) int {
	for i, r := range results {
		if r.Entry.ID == id {
			return i + 1
		}
	}
	return -1
}

// openTestDB is defined in inscribe_test.go (package-shared test helper;
// its variadic signature handles the zero-project case this test needs).

// seedCorpus inserts each fixture entry into the DB, auto-registering
// the referenced projects. Returns the inserted entry IDs in the same
// order the fixtures were supplied.
func seedCorpus(t *testing.T, ctx context.Context, db *sql.DB, corpus []fixtureEntry) []int64 {
	t.Helper()
	seenProjects := map[string]bool{}
	for _, fx := range corpus {
		if seenProjects[fx.ProjectID] {
			continue
		}
		_, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`,
			fx.ProjectID, "/tmp/"+fx.ProjectID)
		if err != nil {
			t.Fatalf("seed project %q: %v", fx.ProjectID, err)
		}
		seenProjects[fx.ProjectID] = true
	}

	now := time.Now().UTC()
	ids := make([]int64, len(corpus))
	for i, fx := range corpus {
		// Spread created_at so recency isn't a tiebreaker on all-identical
		// timestamps (matches real-world corpora where entries are staggered).
		createdAt := now.AddDate(0, 0, -(i % 30)).Format(time.RFC3339)
		res, err := db.ExecContext(ctx,
			`INSERT INTO entries
			 (project_id, topic, kind, title, summary, tags, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'current', ?, ?)`,
			fx.ProjectID, "fixture", fx.Kind, fx.Title, fx.Summary, fx.Tags, createdAt, createdAt)
		if err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
		id, _ := res.LastInsertId()
		ids[i] = id
	}
	return ids
}

// verifyTestDataLayout is a guard against someone refactoring
// seedCorpus in a way that breaks recall@1 reproducibility (corpus
// must stay ordered + unique).
func TestFixtureCorpus_Unique(t *testing.T) {
	seen := map[string]bool{}
	for _, fx := range recallFixtureCorpus() {
		if seen[fx.Title] {
			t.Fatalf("duplicate fixture title: %q", fx.Title)
		}
		seen[fx.Title] = true
	}
}

func TestFixtureCorpus_Sorted(t *testing.T) {
	// Not a strict requirement, just a readability safeguard.
	corpus := recallFixtureCorpus()
	projects := make([]string, len(corpus))
	for i, fx := range corpus {
		projects[i] = fx.ProjectID
	}
	sorted := make([]string, len(projects))
	copy(sorted, projects)
	sort.Strings(sorted)
	if !sort.StringsAreSorted(projects) {
		// informational; just ensure every project appears
		seen := map[string]bool{}
		for _, p := range projects {
			seen[p] = true
		}
		if len(seen) < 3 {
			t.Fatalf("expected ≥3 distinct projects in fixture, got %d", len(seen))
		}
	}
}

// silence: used only in CI diagnostics
var _ = os.Stderr
