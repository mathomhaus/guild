package lore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

func resetCoverageGate(t *testing.T) {
	t.Helper()
	coverageGate.Store(nil)
	liveCoverageQueryCount.Store(0)
}

func upsertMeta(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		t.Fatalf("upsert meta %s=%s: %v", key, value, err)
	}
}

func seedVectorsForEntries(t *testing.T, db *sql.DB, entryIDs []int64) {
	t.Helper()
	now := time.Now().Unix()
	for _, id := range entryIDs {
		_, err := db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO lore_vectors
			   (entry_id, model_id, dim, vec, encoded_at, content_hash)
			 VALUES (?, 'test-model', 4, X'00000000', ?, 'testhash')`,
			id, now,
		)
		if err != nil {
			t.Fatalf("insert lore_vectors for entry %d: %v", id, err)
		}
	}
}

// TestReadCoverage_LiveSQLIgnoresStaleMeta verifies that appraise's
// coverage gate uses live COUNT(*) rather than drifted meta counters.
func TestReadCoverage_LiveSQLIgnoresStaleMeta(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	defer func() { _ = db.Close() }()
	resetCoverageGate(t)

	ids := seedCorpus(t, ctx, db, []fixtureEntry{
		{"p", "research", "alpha vector topic", "summary alpha", "t"},
		{"p", "research", "beta vector topic", "summary beta", "t"},
		{"p", "research", "gamma vector topic", "summary gamma", "t"},
		{"p", "research", "delta vector topic", "summary delta", "t"},
		{"p", "research", "epsilon vector topic", "summary epsilon", "t"},
	})
	seedVectorsForEntries(t, db, ids)

	// Simulate the drift scenario from issue #76: vectors are healthy
	// but meta counters report 61% coverage.
	upsertMeta(t, db, "embedder_state", "enabled")
	upsertMeta(t, db, "vector_coverage_num", "1")
	upsertMeta(t, db, "vector_coverage_den", "10")
	upsertMeta(t, db, "vector_epoch", "7")

	state, cov, err := readCoverage(ctx, db)
	if err != nil {
		t.Fatalf("readCoverage: %v", err)
	}
	if state != "enabled" {
		t.Fatalf("state = %q, want enabled", state)
	}
	if cov < CoverageThreshold {
		t.Fatalf("coverage = %v, want >= %v (live 5/5 should clear gate)", cov, CoverageThreshold)
	}
}

// TestReadCoverage_EpochCacheAvoidsRepeatedCounts verifies that
// consecutive appraise reads at the same epoch do not re-run COUNT(*).
func TestReadCoverage_EpochCacheAvoidsRepeatedCounts(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	defer func() { _ = db.Close() }()
	resetCoverageGate(t)

	ids := seedCorpus(t, ctx, db, []fixtureEntry{
		{"p", "research", "cache test entry one", "summary one", "t"},
		{"p", "research", "cache test entry two", "summary two", "t"},
	})
	seedVectorsForEntries(t, db, ids)
	upsertMeta(t, db, "embedder_state", "enabled")
	upsertMeta(t, db, "vector_epoch", "3")

	if _, _, err := readCoverage(ctx, db); err != nil {
		t.Fatalf("first readCoverage: %v", err)
	}
	if got := liveCoverageQueryCount.Load(); got != 1 {
		t.Fatalf("after first read: liveCoverageQueryCount = %d, want 1", got)
	}

	if _, _, err := readCoverage(ctx, db); err != nil {
		t.Fatalf("second readCoverage: %v", err)
	}
	if got := liveCoverageQueryCount.Load(); got != 1 {
		t.Fatalf("after second read: liveCoverageQueryCount = %d, want 1 (cache hit)", got)
	}

	// Bump epoch: cache miss should run COUNT(*) again.
	upsertMeta(t, db, "vector_epoch", "4")
	if _, _, err := readCoverage(ctx, db); err != nil {
		t.Fatalf("third readCoverage: %v", err)
	}
	if got := liveCoverageQueryCount.Load(); got != 2 {
		t.Fatalf("after epoch bump: liveCoverageQueryCount = %d, want 2", got)
	}
}

// TestAppraiseRRF_EngagesDespiteStaleMeta verifies the RRF path stays
// active when live vector coverage clears the gate even though meta
// counters are stale.
func TestAppraiseRRF_EngagesDespiteStaleMeta(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	defer func() { _ = db.Close() }()
	resetCoverageGate(t)

	ids := seedCorpus(t, ctx, db, []fixtureEntry{
		{"p", "research", "semantic arm gate sentinel alpha", "summary", "t"},
		{"p", "research", "semantic arm gate sentinel beta", "summary", "t"},
	})
	seedVectorsForEntries(t, db, ids)
	upsertMeta(t, db, "embedder_state", "enabled")
	upsertMeta(t, db, "vector_coverage_num", "1")
	upsertMeta(t, db, "vector_coverage_den", "100")
	upsertMeta(t, db, "vector_epoch", "1")

	embedDeps := &EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "test-model",
	}

	params := &AppraiseParams{
		Query:   "semantic arm gate sentinel",
		Project: "p",
		Now:     time.Now().UTC(),
		Embed:   embedDeps,
	}

	out, handled, err := appraiseRRF(ctx, db, params, params.Now, DefaultScoring(), 5)
	if err != nil {
		t.Fatalf("appraiseRRF: %v", err)
	}
	if !handled {
		t.Fatal("appraiseRRF: handled=false, want true when live coverage clears gate")
	}
	if out == nil {
		t.Fatal("appraiseRRF: nil output")
	}
}
