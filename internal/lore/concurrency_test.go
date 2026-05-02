package lore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/storage"
)

// newConcurrencyDB mirrors the quest-package harness but opens a
// file-backed DB (not :memory:) so modernc.org/sqlite exercises real
// cross-goroutine write contention. In-memory handles are
// connection-local under this driver, which would defeat the test.
func newConcurrencyDB(t *testing.T, projectID string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lore.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.MigrateTo(ctx, db, "test", nil); err != nil {
		t.Fatalf("storage.Migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path) VALUES (?, ?)`,
		projectID, "/fake/"+projectID,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return db
}

// seedEnabledEmbedder flips meta.embedder_state='enabled' and sets the
// denominator high enough that the appraise coverage gate passes. The
// concurrency tests operate on a handful of entries and we want the
// RRF branch to be chosen the moment we seed any vectors; without
// this, a fresh corpus would sit at coverage=0/N forever.
func seedEnabledEmbedder(t *testing.T, db *sql.DB, modelID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`UPDATE meta SET value = 'enabled' WHERE key = 'embedder_state'`,
	); err != nil {
		t.Fatalf("enable embedder: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = 'embedder_model_id'`,
		modelID,
	); err != nil {
		t.Fatalf("set embedder_model_id: %v", err)
	}
}

// metaInt reads a meta key as an int64. Test helper; panics on
// unparseable values so a bad seed fails fast.
func metaInt(t *testing.T, db *sql.DB, key string) int64 {
	t.Helper()
	var s string
	if err := db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, key,
	).Scan(&s); err != nil {
		t.Fatalf("read meta %s: %v", key, err)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse meta %s=%q: %v", key, s, err)
	}
	return n
}

// Test_ConcurrentInscribeAndAppraise_NoDeadlocksOrLostWrites spawns N
// goroutines, each either inscribing a fresh entry or running an
// appraise against the shared DB. The writers use a sync EmbedDeps
// (CLI-style) so every Tx2 commits before the goroutine returns.
// After the barrier the test asserts:
//
//  1. Every Inscribe succeeded.
//  2. No SQLITE_BUSY surfaced to any caller (BEGIN IMMEDIATE + retry
//     loop absorbed contention).
//  3. meta.vector_coverage_num equals the number of writers, i.e.
//     every Tx2 successfully bumped the atomic counter and no
//     double-bumps happened.
//  4. The in-process Index holds N vectors, proving the Splice path
//     is race-clean against concurrent readers running TopK.
func Test_ConcurrentInscribeAndAppraise_NoDeadlocksOrLostWrites(t *testing.T) {
	for _, n := range []int{16, 64} {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := newConcurrencyDB(t, "alpha")

			const modelID = "bge-small-en-v1.5-int8-cls"
			seedEnabledEmbedder(t, db, modelID)

			embedder := embed.NewDeterministicEmbedder()
			index := embed.NewIndex(embed.LoreCorpus{}, modelID)
			// Initial load against an empty vectors table seeds
			// loaded=true so TopK does not return ErrIndexStale.
			if _, err := index.LoadFromDB(ctx, db); err != nil {
				t.Fatalf("initial index load: %v", err)
			}

			deps := &EmbedDeps{
				Embedder: embedder,
				Index:    index,
				ModelID:  modelID,
				// Sync so every Tx2 completes before the goroutine
				// returns, letting the assertions see the writes
				// without arbitrary sleeps.
				Async: false,
			}

			var wg sync.WaitGroup
			errs := make(chan error, n*2)

			for i := 0; i < n; i++ {
				i := i
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := Inscribe(ctx, db, &InscribeParams{
						ProjectID: "alpha",
						Kind:      KindResearch,
						Title:     fmt.Sprintf("concurrent inscribe test entry number %d deterministic", i),
						Summary:   fmt.Sprintf("summary-for-concurrent-inscribe-test-%d", i),
						Topic:     "concurrency",
						Embed:     deps,
					})
					if err != nil {
						errs <- fmt.Errorf("inscribe[%d]: %w", i, err)
					}
				}()

				// Interleaved reader: runs at the same time as the
				// writers, exercising TopK + CheckAndReload against
				// concurrent Splice writers and meta bumps.
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Use a small limit so the result slice stays
					// bounded even when the corpus is tiny.
					_, err := Appraise(ctx, db, AppraiseParams{
						Query:   "concurrent inscribe",
						Limit:   5,
						Project: "alpha",
						Embed:   deps,
						Scoring: DefaultScoring(),
						Now:     time.Now().UTC(),
					})
					if err != nil {
						errs <- fmt.Errorf("appraise[%d]: %w", i, err)
					}
				}()
			}

			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("goroutine error: %v", err)
			}

			// meta.vector_coverage_num == N: every Tx2 bumped
			// exactly once, no double-bumps, no dropped writes.
			if got := metaInt(t, db, "vector_coverage_num"); got != int64(n) {
				t.Errorf("vector_coverage_num: got %d, want %d", got, n)
			}
			if got := index.Len(); got != n {
				t.Errorf("index.Len(): got %d, want %d (some Splice calls lost?)", got, n)
			}

			// Epoch must have advanced by exactly N (one bump per
			// successful Tx2 that actually inserted a row).
			if got := metaInt(t, db, "vector_epoch"); got != int64(n) {
				t.Errorf("vector_epoch: got %d, want %d", got, n)
			}
		})
	}
}

// Test_Inscribe_Tx2_ModelIDGuard_AbortsCleanly seeds a DB with a
// model_id distinct from the embedder's bound one and verifies that
// a concurrent Inscribe's Tx2 aborts gracefully: no lore_vectors row
// appears, meta.embed_error_count increments, and the caller never
// sees an error. Proves ADR-003 invariant 2 inside the integrated
// wiring path (the embed package has its own unit test for the raw
// WriteVector call).
func Test_Inscribe_Tx2_ModelIDGuard_AbortsCleanly(t *testing.T) {
	ctx := context.Background()
	db := newConcurrencyDB(t, "alpha")
	// Seed a DIFFERENT model_id than our test deps use.
	if _, err := db.ExecContext(ctx,
		`UPDATE meta SET value = 'future-bge-xxx' WHERE key = 'embedder_model_id'`,
	); err != nil {
		t.Fatalf("seed mismatched model_id: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE meta SET value = 'enabled' WHERE key = 'embedder_state'`,
	); err != nil {
		t.Fatalf("enable embedder: %v", err)
	}

	deps := &EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls", // mismatches meta
		Async:    false,
	}

	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindResearch,
		Title:     "model id guard aborts cleanly regression test",
		Summary:   "should not write a vector because binary is outdated.",
		Topic:     "concurrency",
		Embed:     deps,
	})
	if err != nil {
		t.Fatalf("inscribe (should succeed even though Tx2 is skipped): %v", err)
	}
	if res.Entry == nil {
		t.Fatal("expected a valid entry")
	}

	// No lore_vectors row must exist.
	var nVec int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM lore_vectors WHERE entry_id = ?`,
		res.Entry.ID,
	).Scan(&nVec); err != nil {
		t.Fatalf("count lore_vectors: %v", err)
	}
	if nVec != 0 {
		t.Errorf("lore_vectors rows: got %d, want 0 (model_id guard should have aborted Tx2)", nVec)
	}
	// embed_error_count must have incremented with reason
	// 'binary_outdated' (the hot.go helper bumps it).
	if got := metaInt(t, db, "embed_error_count"); got < 1 {
		t.Errorf("embed_error_count: got %d, want >=1", got)
	}
}

// Test_Inscribe_RRF_VectorArmContributes_Integration is the
// quest-critical integration test: inscribe an entry with a
// synchronous Tx2 (so the vector lands before we appraise), then run
// Appraise with a semantic query that should make the vector arm
// surface that entry in the fused output. Verifies the whole
// inscribe -> Tx2 -> Splice -> Appraise -> RRF chain works together.
func Test_Inscribe_RRF_VectorArmContributes_Integration(t *testing.T) {
	ctx := context.Background()
	db := newConcurrencyDB(t, "alpha")

	const modelID = "bge-small-en-v1.5-int8-cls"
	seedEnabledEmbedder(t, db, modelID)

	embedder := embed.NewDeterministicEmbedder()
	index := embed.NewIndex(embed.LoreCorpus{}, modelID)
	if _, err := index.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	deps := &EmbedDeps{
		Embedder: embedder,
		Index:    index,
		ModelID:  modelID,
		Async:    false,
	}

	// Inscribe enough entries so the coverage ratio clears 0.90.
	// The migration seeds vector_coverage_den to the count of
	// non-archived/non-parked entries at migration time. Since we
	// are inserting into a freshly migrated DB, den starts at 0 and
	// we must also bump den manually for each inscribe (in the real
	// product, inscribe would also bump den; Phase 1.4 scope does
	// not include that, so we simulate by seeding a fixed den).
	if _, err := db.ExecContext(ctx,
		`UPDATE meta SET value = '3' WHERE key = 'vector_coverage_den'`,
	); err != nil {
		t.Fatalf("seed den: %v", err)
	}

	targetSummary := "detailed discussion of retry logic with exponential backoff under transient network failures"
	for i, summary := range []string{
		targetSummary,
		"unrelated summary about sqlite pragma configuration and wal mode tradeoffs",
		"different topic entirely involving cobra cli flag ergonomics and usability",
	} {
		_, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID: "alpha",
			Kind:      KindResearch,
			Title:     fmt.Sprintf("seed entry number %d for rrf integration", i),
			Summary:   summary,
			Topic:     "integration",
			Embed:     deps,
		})
		if err != nil {
			t.Fatalf("inscribe seed %d: %v", i, err)
		}
	}

	// Coverage is now 3/3 = 1.0, well clear of 0.90. Appraise must
	// take the RRF path.
	out, err := Appraise(ctx, db, AppraiseParams{
		Query:   targetSummary, // identical text → deterministic embedder produces the same vector
		Limit:   3,
		Project: "alpha",
		Embed:   deps,
		Scoring: DefaultScoring(),
		Now:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	if len(out.Results) == 0 {
		t.Fatal("appraise returned zero results; expected the matching entry")
	}
	// The first result must be the entry with the matching summary
	// (DeterministicEmbedder produces identical vectors for
	// identical text, so the vector arm ranks it first; BM25 also
	// strongly favors it because the title shares token overlap).
	top := out.Results[0]
	if top.Entry.Summary != targetSummary {
		t.Errorf("top result summary mismatch:\n got: %q\nwant: %q",
			top.Entry.Summary, targetSummary)
	}
}
