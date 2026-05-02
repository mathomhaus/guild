// Backfill tests: spin up a throwaway lore.db, seed some entries, run
// the backfill against a DeterministicEmbedder (so no ORT deps), and
// assert idempotence, coverage tracking, and identity-invalidate.

package embed

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/storage"
)

// newTestDB returns an in-memory SQLite with every migration applied.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.Migrate(ctx, db, "test"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedEntries inserts n entries with distinct summaries. Returns the
// inserted IDs.
func seedEntries(t *testing.T, db *sql.DB, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		r, err := db.ExecContext(ctx, `
			INSERT INTO entries (project_id, topic, kind, title, summary, tags, status)
			VALUES ('p', 't', 'observation', ?, ?, '', 'current')
		`, "title-"+strconv.Itoa(i), "summary body number "+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
		id, _ := r.LastInsertId()
		ids = append(ids, id)
	}
	return ids
}

func readMetaInt(t *testing.T, db *sql.DB, key string) int64 {
	t.Helper()
	var s sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	if !s.Valid || s.String == "" {
		return 0
	}
	n, err := strconv.ParseInt(s.String, 10, 64)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", key, s.String, err)
	}
	return n
}

func TestBackfill_EmbedsPending(t *testing.T) {
	db := newTestDB(t)
	ids := seedEntries(t, db, 5)

	res, err := Backfill(context.Background(), BackfillOptions{
		DB:       db,
		Embedder: NewDeterministicEmbedder(),
		ModelID:  "test",
	})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Total != len(ids) || res.Embedded != len(ids) || res.Failed != 0 {
		t.Errorf("counts: total=%d embedded=%d failed=%d", res.Total, res.Embedded, res.Failed)
	}
	if res.Epoch <= 0 {
		t.Errorf("epoch not bumped: %d", res.Epoch)
	}
	// Every entry should have vector_state='indexed' and a row in lore_vectors.
	var vecRows int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM lore_vectors`).Scan(&vecRows); err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	if vecRows != len(ids) {
		t.Errorf("vector rows: got %d want %d", vecRows, len(ids))
	}
	var pendingCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM entries WHERE vector_state <> 'indexed'`).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingCount != 0 {
		t.Errorf("entries left pending: %d", pendingCount)
	}
	if got := readMetaInt(t, db, "vector_coverage_num"); got != int64(len(ids)) {
		t.Errorf("coverage_num: got %d want %d", got, len(ids))
	}
}

// TestBackfill_Idempotent: re-running yields zero new embeddings.
func TestBackfill_Idempotent(t *testing.T) {
	db := newTestDB(t)
	seedEntries(t, db, 3)

	ctx := context.Background()
	first, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: "t"})
	if err != nil {
		t.Fatalf("first Backfill: %v", err)
	}
	epochAfterFirst := first.Epoch

	second, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: "t"})
	if err != nil {
		t.Fatalf("second Backfill: %v", err)
	}
	if second.Total != 0 {
		t.Errorf("second Backfill saw Total=%d, want 0 (idempotence)", second.Total)
	}
	if second.Embedded != 0 {
		t.Errorf("second Backfill embedded %d, want 0", second.Embedded)
	}
	// Epoch still bumps on empty runs (design choice) but never decreases.
	if second.Epoch < epochAfterFirst {
		t.Errorf("epoch went backward: %d < %d", second.Epoch, epochAfterFirst)
	}
}

// TestBackfill_ExcludesArchivedParked: entries with status in
// ('archived','parked') never get a vector row.
func TestBackfill_ExcludesArchivedParked(t *testing.T) {
	db := newTestDB(t)
	ids := seedEntries(t, db, 3)
	// Flip first → archived, second → parked.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE entries SET status = 'archived' WHERE id = ?`, ids[0]); err != nil {
		t.Fatalf("flip archived: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`UPDATE entries SET status = 'parked' WHERE id = ?`, ids[1]); err != nil {
		t.Fatalf("flip parked: %v", err)
	}

	res, err := Backfill(context.Background(), BackfillOptions{
		DB:       db,
		Embedder: NewDeterministicEmbedder(),
		ModelID:  "t",
	})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Total != 1 || res.Embedded != 1 {
		t.Errorf("expected exactly the third entry to backfill: total=%d embedded=%d", res.Total, res.Embedded)
	}
}

// TestBackfill_NullEmbedder_ShortCircuits: NullEmbedder returns
// ErrEmbedderDisabled immediately, and Backfill surfaces that error
// wrapping ErrEmbedderDisabled.
func TestBackfill_NullEmbedder_ShortCircuits(t *testing.T) {
	db := newTestDB(t)
	seedEntries(t, db, 3)
	_, err := Backfill(context.Background(), BackfillOptions{
		DB:       db,
		Embedder: NewNullEmbedder(),
		ModelID:  "t",
	})
	if !errors.Is(err, ErrEmbedderDisabled) {
		t.Errorf("want ErrEmbedderDisabled, got %v", err)
	}
}

// TestInvalidate_DropsVectorsAndFlipsState: after Invalidate, every
// lore_vectors row is gone and every active entry is vector_state=pending.
func TestInvalidate_DropsVectorsAndFlipsState(t *testing.T) {
	db := newTestDB(t)
	ids := seedEntries(t, db, 3)
	ctx := context.Background()
	if _, err := Backfill(ctx, BackfillOptions{DB: db, Embedder: NewDeterministicEmbedder(), ModelID: "t"}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// Archive one entry so we can confirm it is NOT flipped back.
	if _, err := db.ExecContext(ctx,
		`UPDATE entries SET status='archived' WHERE id = ?`, ids[0]); err != nil {
		t.Fatalf("archive: %v", err)
	}
	newIdent := ManifestIdentity{ModelID: "new", TokenizerHash: "abc", RuntimeVersion: "v2", Dim: Dim}
	if err := Invalidate(ctx, db, LoreCorpus{}, newIdent); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	var vecRows int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lore_vectors`).Scan(&vecRows)
	if vecRows != 0 {
		t.Errorf("vectors not cleared: %d", vecRows)
	}
	var pending int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE vector_state = 'pending' AND status NOT IN ('archived','parked')`,
	).Scan(&pending)
	if pending != 2 {
		t.Errorf("expected 2 active pending entries, got %d", pending)
	}
	var storedModel string
	_ = db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='embedder_model_id'`).Scan(&storedModel)
	if storedModel != "new" {
		t.Errorf("model_id not updated: %q", storedModel)
	}
	if got := readMetaInt(t, db, "vector_coverage_num"); got != 0 {
		t.Errorf("coverage_num not reset: %d", got)
	}
}

func TestIdentityChanged(t *testing.T) {
	bound := ManifestIdentity{ModelID: "m1", TokenizerHash: "h1"}
	cases := []struct {
		name   string
		stored ManifestIdentity
		want   bool
	}{
		{"empty stored → same", ManifestIdentity{}, false},
		{"model drift → changed", ManifestIdentity{ModelID: "m2", TokenizerHash: "h1"}, true},
		{"same model empty hash → same", ManifestIdentity{ModelID: "m1", TokenizerHash: ""}, false},
		{"hash drift on same model → changed", ManifestIdentity{ModelID: "m1", TokenizerHash: "h2"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentityChanged(tc.stored, bound); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestBackfill_ProgressThreshold(t *testing.T) {
	db := newTestDB(t)
	// Below threshold → no progress lines.
	seedEntries(t, db, 5)
	var buf bytes.Buffer
	_, err := Backfill(context.Background(), BackfillOptions{
		DB:                db,
		Embedder:          NewDeterministicEmbedder(),
		ModelID:           "t",
		ProgressOut:       &buf,
		ProgressThreshold: 100,
		ProgressEvery:     1,
	})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if strings.Contains(buf.String(), "backfill: [") {
		t.Errorf("progress rendered below threshold: %q", buf.String())
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)

// setMetaInt writes key=value into the meta table, overwriting any existing row.
func setMetaInt(t *testing.T, db *sql.DB, key string, val int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, strconv.FormatInt(val, 10),
	); err != nil {
		t.Fatalf("setMetaInt(%q, %d): %v", key, val, err)
	}
}

// TestReconcileDen_FixesDrift verifies that ReconcileDen corrects a stale
// vector_coverage_den by setting it to the live COUNT(*) of active entries.
func TestReconcileDen_FixesDrift(t *testing.T) {
	db := newTestDB(t)
	seedEntries(t, db, 5)

	// Force den to a wrong value to simulate drift.
	setMetaInt(t, db, "vector_coverage_den", 2)
	if got := readMetaInt(t, db, "vector_coverage_den"); got != 2 {
		t.Fatalf("precondition: den=%d want 2", got)
	}

	if err := ReconcileDen(context.Background(), db, LoreCorpus{}); err != nil {
		t.Fatalf("ReconcileDen: %v", err)
	}

	if got := readMetaInt(t, db, "vector_coverage_den"); got != 5 {
		t.Errorf("den after reconcile: got %d want 5", got)
	}
}

// TestBackfill_ReconcilesDenBeforeWriting verifies that Backfill fixes a
// stale den before writing vectors, so num <= den holds after the run.
func TestBackfill_ReconcilesDenBeforeWriting(t *testing.T) {
	db := newTestDB(t)
	seedEntries(t, db, 5)

	// Force den to 1 (stale low value) to simulate the QUEST-220 drift scenario.
	setMetaInt(t, db, "vector_coverage_den", 1)

	res, err := Backfill(context.Background(), BackfillOptions{
		DB:       db,
		Embedder: NewDeterministicEmbedder(),
		ModelID:  "test",
	})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Embedded != 5 {
		t.Errorf("embedded: got %d want 5", res.Embedded)
	}

	num := readMetaInt(t, db, "vector_coverage_num")
	den := readMetaInt(t, db, "vector_coverage_den")
	if num > den {
		t.Errorf("invariant violated: num=%d > den=%d", num, den)
	}
	if den != 5 {
		t.Errorf("den after backfill: got %d want 5 (reconciled)", den)
	}
	if num != 5 {
		t.Errorf("num after backfill: got %d want 5", num)
	}
}

// TestReconcileDen_ExcludesArchivedParked verifies that archived and parked
// entries are not counted in the denominator.
func TestReconcileDen_ExcludesArchivedParked(t *testing.T) {
	db := newTestDB(t)
	ids := seedEntries(t, db, 4)
	ctx := context.Background()

	// Archive one, park one.
	if _, err := db.ExecContext(ctx, `UPDATE entries SET status = 'archived' WHERE id = ?`, ids[0]); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE entries SET status = 'parked' WHERE id = ?`, ids[1]); err != nil {
		t.Fatalf("park: %v", err)
	}

	if err := ReconcileDen(ctx, db, LoreCorpus{}); err != nil {
		t.Fatalf("ReconcileDen: %v", err)
	}

	// Only 2 of the 4 are active.
	if got := readMetaInt(t, db, "vector_coverage_den"); got != 2 {
		t.Errorf("den after reconcile: got %d want 2 (active only)", got)
	}
}

// TestCoverageInvariant_NumNeverExceedsDen seeds entries, backfills, and
// asserts that num <= den at every step (regression for QUEST-220).
func TestCoverageInvariant_NumNeverExceedsDen(t *testing.T) {
	db := newTestDB(t)
	seedEntries(t, db, 7)

	res, err := Backfill(context.Background(), BackfillOptions{
		DB:       db,
		Embedder: NewDeterministicEmbedder(),
		ModelID:  "t",
	})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Embedded != 7 {
		t.Errorf("embedded: got %d want 7", res.Embedded)
	}

	num := readMetaInt(t, db, "vector_coverage_num")
	den := readMetaInt(t, db, "vector_coverage_den")
	if num > den {
		t.Errorf("QUEST-220 regression: num=%d > den=%d (coverage > 100%%)", num, den)
	}
	if den != 7 {
		t.Errorf("den: got %d want 7", den)
	}
	if num != 7 {
		t.Errorf("num: got %d want 7", num)
	}
}
