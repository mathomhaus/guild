package embed

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// canonModelID mirrors the seed value in migration 003 so tests bind
// an Index to the same identity the schema expects. Keeping a local
// constant here avoids reaching into internal/lore (which would invert
// the embed package's hexagonal position).
const canonModelID = "bge-small-en-v1.5-int8-cls"

// TestIndex_EmptyIndex_TopKReturnsStale asserts that TopK on a freshly
// constructed Index returns ErrIndexStale; callers are expected to
// LoadFromDB at startup and only query after load.
func TestIndex_EmptyIndex_TopKReturnsStale(t *testing.T) {
	idx := NewIndex(LoreCorpus{}, canonModelID)
	q := make([]int8, VecDim)
	_, err := idx.TopK(q, 10)
	if !errors.Is(err, ErrIndexStale) {
		t.Fatalf("got %v, want ErrIndexStale", err)
	}
}

// TestIndex_TopK_WrongQueryShapeIsTypedError: guards against callers
// passing a truncated query vector.
func TestIndex_TopK_WrongQueryShapeIsTypedError(t *testing.T) {
	idx := NewIndex(LoreCorpus{}, canonModelID)
	_, err := idx.TopK(make([]int8, VecDim-1), 10)
	if !errors.Is(err, ErrQueryShape) {
		t.Fatalf("got %v, want ErrQueryShape", err)
	}
	_, err = idx.TopK(nil, 10)
	if !errors.Is(err, ErrQueryShape) {
		t.Fatalf("nil qvec: got %v, want ErrQueryShape", err)
	}
}

// TestIndex_LoadFromDB_EmptyTableOK: an empty lore_vectors table loads
// cleanly; loaded flips true and TopK returns an empty slice rather
// than ErrIndexStale.
func TestIndex_LoadFromDB_EmptyTableOK(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, canonModelID)
	n, err := idx.LoadFromDB(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if n != 0 {
		t.Fatalf("loaded %d, want 0", n)
	}
	hits, err := idx.TopK(make([]int8, VecDim), 10)
	if err != nil {
		t.Fatalf("TopK on empty: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("TopK returned %d hits, want 0", len(hits))
	}
}

// TestIndex_LoadFromDB_ReadsMatchingModelOnly: rows written with a
// different model_id are skipped at load time. Guards the versioning
// invariant (ADR-003): a corpus must not mix old and new vectors in
// the in-memory index.
func TestIndex_LoadFromDB_ReadsMatchingModelOnly(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)

	// Two entry rows with distinct IDs, two vectors under the canonical
	// model_id, plus one row under a stale model_id that must not leak
	// into the loaded corpus.
	mustSeedEntry(t, db, 1, "canonical entry 1")
	mustSeedEntry(t, db, 2, "canonical entry 2")
	mustSeedEntry(t, db, 3, "stale entry")

	mustInsertVec(t, db, 1, canonModelID, Quantize(deterministicUnitVec(100)))
	mustInsertVec(t, db, 2, canonModelID, Quantize(deterministicUnitVec(101)))
	mustInsertVec(t, db, 3, "bge-small-v1-old", Quantize(deterministicUnitVec(102)))

	idx := NewIndex(LoreCorpus{}, canonModelID)
	n, err := idx.LoadFromDB(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if n != 2 {
		t.Fatalf("loaded %d, want 2", n)
	}
	if idx.Len() != 2 {
		t.Fatalf("Len = %d, want 2", idx.Len())
	}
}

// TestIndex_LoadFromDB_ModelIdentityMismatch returns ErrModelMismatch
// when the bound model_id disagrees with meta.embedder_model_id. The
// caller's correct response is "bump the binary or re-run guild init",
// not silently query a mismatched corpus.
func TestIndex_LoadFromDB_ModelIdentityMismatch(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, "some-other-model")
	_, err := idx.LoadFromDB(ctx, db)
	if !errors.Is(err, ErrModelMismatch) {
		t.Fatalf("got %v, want ErrModelMismatch", err)
	}
}

// TestIndex_LoadFromDB_SkipsMalformedRows: a BLOB of the wrong length
// is logged and skipped; the rest of the corpus still loads. Matches
// the ADR-003 defensive posture that Tx2 refuses to write bad blobs,
// so any mismatch in storage indicates corruption and the reader must
// not crash on it.
func TestIndex_LoadFromDB_SkipsMalformedRows(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)

	mustSeedEntry(t, db, 1, "good")
	mustSeedEntry(t, db, 2, "bad")
	mustSeedEntry(t, db, 3, "good")
	mustInsertVec(t, db, 1, canonModelID, Quantize(deterministicUnitVec(1)))
	mustInsertVec(t, db, 2, canonModelID, make([]int8, 128)) // wrong length
	mustInsertVec(t, db, 3, canonModelID, Quantize(deterministicUnitVec(2)))

	idx := NewIndex(LoreCorpus{}, canonModelID)
	n, err := idx.LoadFromDB(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if n != 2 {
		t.Fatalf("loaded %d, want 2", n)
	}
}

// TestIndex_TopK_RanksExactMatchFirst: the TopK result for a query
// equal to one of the stored vectors must rank that vector first.
// Smoke test that the cosine math and the slice access agree.
func TestIndex_TopK_RanksExactMatchFirst(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)

	// Seed 8 distinct vectors under the canonical model.
	const n = 8
	refs := make([][]int8, n)
	for i := 0; i < n; i++ {
		mustSeedEntry(t, db, int64(i+1), "entry")
		refs[i] = Quantize(deterministicUnitVec(int64(200 + i)))
		mustInsertVec(t, db, int64(i+1), canonModelID, refs[i])
	}

	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}

	// Query with the exact vector for entry 5.
	hits, err := idx.TopK(refs[4], 3)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("len(hits)=%d, want 3", len(hits))
	}
	if hits[0].EntryID != 5 {
		t.Fatalf("hits[0].EntryID = %d, want 5", hits[0].EntryID)
	}
	// Top hit's score must strictly dominate the rest.
	if hits[0].Score < hits[1].Score || hits[0].Score < hits[2].Score {
		t.Fatalf("score order broken: %+v", hits)
	}
}

// TestIndex_TopK_KLargerThanCorpus: TopK clamps k to len(index) rather
// than slicing past the end.
func TestIndex_TopK_KLargerThanCorpus(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	mustSeedEntry(t, db, 1, "e1")
	mustInsertVec(t, db, 1, canonModelID, Quantize(deterministicUnitVec(1)))
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	hits, err := idx.TopK(Quantize(deterministicUnitVec(1)), 999)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits)=%d, want 1", len(hits))
	}
}

// TestIndex_TopK_ZeroK: k <= 0 returns an empty non-nil slice (not an
// error). Caller API decision: "give me zero hits" is a legal query.
func TestIndex_TopK_ZeroK(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	mustSeedEntry(t, db, 1, "e1")
	mustInsertVec(t, db, 1, canonModelID, Quantize(deterministicUnitVec(1)))
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	hits, err := idx.TopK(make([]int8, VecDim), 0)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	if hits == nil {
		t.Fatal("hits is nil, want empty slice")
	}
	if len(hits) != 0 {
		t.Fatalf("len(hits)=%d, want 0", len(hits))
	}
}

// TestIndex_TopK_DeterministicTiebreak: two entries with identical
// vectors produce a deterministic ordering (entry_id ascending).
// Important for RRF downstream, which ranks by position.
func TestIndex_TopK_DeterministicTiebreak(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	v := Quantize(deterministicUnitVec(42))
	mustSeedEntry(t, db, 5, "a")
	mustSeedEntry(t, db, 7, "b")
	mustSeedEntry(t, db, 3, "c")
	mustInsertVec(t, db, 5, canonModelID, v)
	mustInsertVec(t, db, 7, canonModelID, v)
	mustInsertVec(t, db, 3, canonModelID, v)

	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	hits, err := idx.TopK(v, 3)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	want := []int64{3, 5, 7}
	for i, w := range want {
		if hits[i].EntryID != w {
			t.Fatalf("hits[%d].EntryID = %d, want %d", i, hits[i].EntryID, w)
		}
	}
}

// TestIndex_CheckAndReload_NoOpWhenEpochUnchanged: CheckAndReload on a
// DB whose epoch has not advanced must be a pure read and return
// reloaded=false. Regression guard for the hot-path check.
func TestIndex_CheckAndReload_NoOpWhenEpochUnchanged(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	mustSeedEntry(t, db, 1, "e1")
	mustInsertVec(t, db, 1, canonModelID, Quantize(deterministicUnitVec(1)))
	// Seed epoch to some non-zero value so cachedEpoch == DB value.
	mustSetEpoch(t, db, 42)

	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if idx.Epoch() != 42 {
		t.Fatalf("cachedEpoch = %d, want 42", idx.Epoch())
	}
	reloaded, err := idx.CheckAndReload(ctx, db)
	if err != nil {
		t.Fatalf("CheckAndReload: %v", err)
	}
	if reloaded {
		t.Fatal("CheckAndReload reloaded on unchanged epoch")
	}
}

// TestIndex_CheckAndReload_ReloadsOnEpochBump: an external writer
// advances meta.vector_epoch and inserts a new vector; CheckAndReload
// must pick up the new row.
func TestIndex_CheckAndReload_ReloadsOnEpochBump(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	mustSeedEntry(t, db, 1, "e1")
	mustInsertVec(t, db, 1, canonModelID, Quantize(deterministicUnitVec(1)))
	mustSetEpoch(t, db, 1)

	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if idx.Len() != 1 {
		t.Fatalf("pre-bump Len = %d, want 1", idx.Len())
	}

	// External writer: insert a new vector and bump the epoch.
	mustSeedEntry(t, db, 2, "e2")
	mustInsertVec(t, db, 2, canonModelID, Quantize(deterministicUnitVec(2)))
	mustSetEpoch(t, db, 2)

	reloaded, err := idx.CheckAndReload(ctx, db)
	if err != nil {
		t.Fatalf("CheckAndReload: %v", err)
	}
	if !reloaded {
		t.Fatal("CheckAndReload: want reloaded=true")
	}
	if idx.Len() != 2 {
		t.Fatalf("post-bump Len = %d, want 2", idx.Len())
	}
	if idx.Epoch() != 2 {
		t.Fatalf("cachedEpoch = %d, want 2", idx.Epoch())
	}
}

// TestIndex_Splice_InsertNew: Splice on an unknown entry_id appends a
// new slot and bumps cachedEpoch.
func TestIndex_Splice_InsertNew(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}

	v := Quantize(deterministicUnitVec(1))
	if err := idx.Splice(1, v, 1); err != nil {
		t.Fatalf("Splice: %v", err)
	}
	if idx.Len() != 1 {
		t.Fatalf("Len = %d, want 1", idx.Len())
	}
	if idx.Epoch() != 1 {
		t.Fatalf("Epoch = %d, want 1", idx.Epoch())
	}
	hits, err := idx.TopK(v, 1)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	if len(hits) != 1 || hits[0].EntryID != 1 {
		t.Fatalf("hits = %+v, want entry 1", hits)
	}
}

// TestIndex_Splice_ReplaceExisting: Splice on a known entry_id updates
// the existing slot in place. Guards against a buggy append that would
// duplicate the entry.
func TestIndex_Splice_ReplaceExisting(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}

	v1 := Quantize(deterministicUnitVec(1))
	v2 := Quantize(deterministicUnitVec(2))
	if err := idx.Splice(7, v1, 1); err != nil {
		t.Fatalf("Splice v1: %v", err)
	}
	if err := idx.Splice(7, v2, 2); err != nil {
		t.Fatalf("Splice v2: %v", err)
	}
	if idx.Len() != 1 {
		t.Fatalf("Len = %d, want 1", idx.Len())
	}
	// Query with v2: must match the replaced vector exactly.
	hits, err := idx.TopK(v2, 1)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	wantScore := cosineInt8(v2, v2)
	if hits[0].Score != wantScore {
		t.Fatalf("score %d, want %d (replaced v1 with v2)", hits[0].Score, wantScore)
	}
}

// TestIndex_Splice_WrongLength: wrong-length vec returns an error;
// does not mutate the index.
func TestIndex_Splice_WrongLength(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if err := idx.Splice(1, make([]int8, 10), 1); err == nil {
		t.Fatal("Splice with short vec: want error, got nil")
	}
	if idx.Len() != 0 {
		t.Fatalf("Len after failed Splice = %d, want 0", idx.Len())
	}
}

// TestIndex_Splice_EpochRegress: passing a newEpoch smaller than the
// cached epoch is a caller bug (likely passed the pre-increment
// value) and returns an error.
func TestIndex_Splice_EpochRegress(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	v := Quantize(deterministicUnitVec(1))
	if err := idx.Splice(1, v, 5); err != nil {
		t.Fatalf("Splice: %v", err)
	}
	if err := idx.Splice(1, v, 4); err == nil {
		t.Fatal("Splice epoch regress: want error, got nil")
	}
}

// TestIndex_Splice_DefensiveCopy: the caller's buffer can be reused
// after Splice returns without corrupting the index.
func TestIndex_Splice_DefensiveCopy(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}

	v := Quantize(deterministicUnitVec(11))
	if err := idx.Splice(1, v, 1); err != nil {
		t.Fatalf("Splice: %v", err)
	}
	// Now mutate the caller's slice.
	for i := range v {
		v[i] = 0
	}
	// Query with the original vector's fresh copy: must still match.
	fresh := Quantize(deterministicUnitVec(11))
	hits, err := idx.TopK(fresh, 1)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	wantScore := cosineInt8(fresh, fresh)
	if hits[0].Score != wantScore {
		t.Fatalf("defensive copy broken: score %d, want %d", hits[0].Score, wantScore)
	}
}

// TestIndex_ConcurrentReadsAndSplice exercises the sync.RWMutex
// contract under `go test -race`: many concurrent TopK readers while a
// single writer splices vectors. No assertions beyond "no race, no
// panic" and that Len increases monotonically as expected.
func TestIndex_ConcurrentReadsAndSplice(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)
	idx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}

	const readers = 8
	const writes = 128

	var (
		done   atomic.Bool
		wg     sync.WaitGroup
		readWg sync.WaitGroup
	)

	qvec := Quantize(deterministicUnitVec(999))

	readWg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readWg.Done()
			for !done.Load() {
				if _, err := idx.TopK(qvec, 8); err != nil && !errors.Is(err, ErrIndexStale) {
					t.Errorf("TopK: %v", err)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= writes; i++ {
			if err := idx.Splice(i, Quantize(deterministicUnitVec(i)), i); err != nil {
				t.Errorf("Splice: %v", err)
				return
			}
		}
	}()

	wg.Wait()
	done.Store(true)
	readWg.Wait()

	if idx.Len() != writes {
		t.Fatalf("final Len = %d, want %d", idx.Len(), writes)
	}
}

// openEmbedTestDB opens a fresh SQLite database, applies every
// migration through 004, seeds the 'p' project row so entry inserts
// in tests can satisfy the project_id FK, and returns the handle.
// Running the full migration sequence matches production exactly and
// catches accidental dependencies on later migrations.
func openEmbedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "embed_index_test.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.MigrateTo(ctx, db, "test", nil); err != nil {
		t.Fatalf("storage.MigrateTo: %v", err)
	}
	// Seed the 'p' project row. Tests reference it via entries.project_id;
	// the path is unique per test because t.TempDir allocates a fresh dir.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path) VALUES ('p', ?)`,
		filepath.Join(t.TempDir(), "proj"),
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return db
}

// mustSeedEntry inserts a row into the entries table with the given
// id. Used to satisfy the lore_vectors.entry_id foreign-key constraint
// in tests that only care about vector-layer behavior.
func mustSeedEntry(t *testing.T, db *sql.DB, id int64, title string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO entries
		(id, project_id, topic, kind, title, summary, status, created_at, updated_at)
		VALUES (?, 'p', 't', 'research', ?, 'summary text',
			'current', ?, ?)
	`, id, title, now, now)
	if err != nil {
		t.Fatalf("seed entry %d: %v", id, err)
	}
}

// mustInsertVec writes a lore_vectors row directly. Keeps tests from
// having to plumb an embedder + Tx2 path to exercise the reader side.
func mustInsertVec(t *testing.T, db *sql.DB, entryID int64, modelID string, vec []int8) {
	t.Helper()
	blob := make([]byte, len(vec))
	for i, v := range vec {
		blob[i] = byte(v)
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT OR REPLACE INTO lore_vectors
		(entry_id, model_id, dim, vec, encoded_at, content_hash)
		VALUES (?, ?, ?, ?, ?, ?)
	`, entryID, modelID, len(vec), blob, time.Now().Unix(), "testhash")
	if err != nil {
		t.Fatalf("insert vec entry=%d: %v", entryID, err)
	}
}

// mustSetEpoch writes meta.vector_epoch to a specific value so tests
// can exercise the CheckAndReload fast path deterministically.
func mustSetEpoch(t *testing.T, db *sql.DB, v int64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE meta SET value = ? WHERE key = 'vector_epoch'`,
		v,
	)
	if err != nil {
		t.Fatalf("set epoch: %v", err)
	}
}
