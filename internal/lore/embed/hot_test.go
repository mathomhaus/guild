package embed

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// hotTestDB opens a fresh migrated DB and inserts one test project
// plus one test entry. Returns the entry id so the caller can drive
// WriteVector against a real row.
func hotTestDB(t *testing.T) (db *sql.DB, entryID int64) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lore.db")
	var err error
	db, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.MigrateTo(ctx, db, "test", nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path) VALUES (?, ?)`, "p", "/fake/p",
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO entries
		  (project_id, topic, kind, title, summary, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "p", "t", "research",
		"write vector unit test entry title one",
		"unit test summary for WriteVector round trip verification",
		"current",
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert entry: %v", err)
	}
	entryID, _ = res.LastInsertId()
	return db, entryID
}

// hotMetaInt reads a meta key as an int64. Test helper.
func hotMetaInt(t *testing.T, db *sql.DB, key string) int64 {
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

// TestWriteVector_HappyPath runs the Tx2 once against a fresh entry,
// then asserts: (a) lore_vectors has one row, (b) vector_state flipped
// to 'indexed', (c) vector_epoch and vector_coverage_num each
// incremented by 1, (d) the returned WriteVectorResult.Vec is the
// quantized vector.
func TestWriteVector_HappyPath(t *testing.T) {
	ctx := context.Background()
	db, entryID := hotTestDB(t)

	deps := HotDeps{
		Embedder: NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
	}
	res, err := WriteVector(ctx, db, deps, entryID, "the summary text to embed")
	if err != nil {
		t.Fatalf("WriteVector: %v", err)
	}
	if !res.Written {
		t.Fatal("Written=false, want true")
	}
	if len(res.Vec) != VecDim {
		t.Errorf("Vec len: got %d, want %d", len(res.Vec), VecDim)
	}

	// lore_vectors has one row.
	var nVec int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM lore_vectors WHERE entry_id = ?`, entryID,
	).Scan(&nVec); err != nil {
		t.Fatalf("count lore_vectors: %v", err)
	}
	if nVec != 1 {
		t.Errorf("lore_vectors rows: got %d, want 1", nVec)
	}

	// vector_state = 'indexed'.
	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT vector_state FROM entries WHERE id = ?`, entryID,
	).Scan(&state); err != nil {
		t.Fatalf("read vector_state: %v", err)
	}
	if state != "indexed" {
		t.Errorf("vector_state: got %q, want indexed", state)
	}

	// Counters bumped exactly once.
	if got := hotMetaInt(t, db, "vector_epoch"); got != 1 {
		t.Errorf("vector_epoch: got %d, want 1", got)
	}
	if got := hotMetaInt(t, db, "vector_coverage_num"); got != 1 {
		t.Errorf("vector_coverage_num: got %d, want 1", got)
	}
}

// TestWriteVector_InsertOrIgnoreNoDoubleBump verifies that a second
// WriteVector against the same entry does NOT double-bump the
// coverage counter. INSERT OR IGNORE collapses the row, and our
// conditional counter bump only fires on a real insert. The epoch
// still advances so other readers refresh.
func TestWriteVector_InsertOrIgnoreNoDoubleBump(t *testing.T) {
	ctx := context.Background()
	db, entryID := hotTestDB(t)

	deps := HotDeps{
		Embedder: NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
	}
	if _, err := WriteVector(ctx, db, deps, entryID, "first"); err != nil {
		t.Fatalf("first WriteVector: %v", err)
	}
	if _, err := WriteVector(ctx, db, deps, entryID, "second"); err != nil {
		t.Fatalf("second WriteVector: %v", err)
	}

	// Still exactly one row.
	var nVec int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM lore_vectors WHERE entry_id = ?`, entryID,
	).Scan(&nVec)
	if nVec != 1 {
		t.Errorf("lore_vectors rows after two writes: got %d, want 1", nVec)
	}
	// coverage_num MUST still be 1 (no double bump).
	if got := hotMetaInt(t, db, "vector_coverage_num"); got != 1 {
		t.Errorf("vector_coverage_num after two writes: got %d, want 1 (INSERT OR IGNORE must not double-bump)", got)
	}
	// epoch bumps both times (2).
	if got := hotMetaInt(t, db, "vector_epoch"); got != 2 {
		t.Errorf("vector_epoch after two writes: got %d, want 2", got)
	}
}

// TestWriteVector_ModelIDMismatch_GracefulAbort verifies ADR-003
// invariant 2: when the embedder's bound model_id disagrees with the
// DB's meta.embedder_model_id, WriteVector aborts cleanly (no row
// inserted, no user error) and bumps embed_error_count so the
// "broken binary" state is visible in health.
func TestWriteVector_ModelIDMismatch_GracefulAbort(t *testing.T) {
	ctx := context.Background()
	db, entryID := hotTestDB(t)

	// Seed a DIFFERENT model_id than our deps claim.
	if _, err := db.ExecContext(ctx,
		`UPDATE meta SET value = 'future-model-v2' WHERE key = 'embedder_model_id'`,
	); err != nil {
		t.Fatalf("seed different model_id: %v", err)
	}

	deps := HotDeps{
		Embedder: NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls", // does NOT match meta
	}
	res, err := WriteVector(ctx, db, deps, entryID, "summary to encode")
	if err != nil {
		t.Fatalf("WriteVector on mismatch: got err %v, want nil", err)
	}
	if res.Written {
		t.Error("Written=true on model_id mismatch, want false")
	}

	// No lore_vectors row must exist.
	var nVec int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM lore_vectors WHERE entry_id = ?`, entryID,
	).Scan(&nVec)
	if nVec != 0 {
		t.Errorf("lore_vectors rows after mismatch: got %d, want 0", nVec)
	}
	// embed_error_count must have incremented.
	if got := hotMetaInt(t, db, "embed_error_count"); got < 1 {
		t.Errorf("embed_error_count after mismatch: got %d, want >=1", got)
	}
}

// TestWriteVector_SplicesIntoIndex verifies that when an Index is
// provided the vector is spliced in-process after commit, so a
// subsequent TopK reflects the write without a LoadFromDB round trip.
func TestWriteVector_SplicesIntoIndex(t *testing.T) {
	ctx := context.Background()
	db, entryID := hotTestDB(t)

	const modelID = "bge-small-en-v1.5-int8-cls"
	idx := NewIndex(LoreCorpus{}, modelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	pre := idx.Len()

	deps := HotDeps{
		Embedder: NewDeterministicEmbedder(),
		Index:    idx,
		ModelID:  modelID,
	}
	if _, err := WriteVector(ctx, db, deps, entryID, "summary-for-splice-test"); err != nil {
		t.Fatalf("WriteVector: %v", err)
	}
	post := idx.Len()
	if post != pre+1 {
		t.Errorf("index.Len after splice: got %d, want %d", post, pre+1)
	}
}

// TestContentHash_IsStable verifies the public ContentHash helper
// produces a stable hex-encoded SHA-256 that lore_update can compare
// against the stored lore_vectors.content_hash.
func TestContentHash_IsStable(t *testing.T) {
	got := ContentHash("hello world")
	// SHA-256 of "hello world" is a well-known value.
	want := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if got != want {
		t.Errorf("ContentHash(\"hello world\") = %q, want %q", got, want)
	}
}
