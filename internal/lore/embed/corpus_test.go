// Parameterized interface tests: exercise every public vector
// operation against LoreCorpus (production adapter) and a FakeCorpus
// fixture (fake_vectors + fake_entities tables with 'fake.'-prefixed
// meta keys). The suite proves three things:
//
//  1. LSP: the algorithms behave equivalently across corpora. Every
//     assertion that holds for LoreCorpus must also hold for
//     FakeCorpus; any asymmetry is an adapter leak in the algorithm.
//
//  2. Prefix isolation: two corpora sharing one *sql.DB never pollute
//     each other's state. A write through LoreCorpus must not change
//     FakeCorpus's Index.Len or meta rows, and vice versa.
//
//  3. Open/Closed: adding FakeCorpus required zero edits to Index,
//     Backfill, WriteVector, or ReadHealthReport (the port is truly
//     orthogonal).
//
// The FakeCorpus adapter exists ONLY for tests. No production code
// references it; the parameterized suite constructs both adapters
// side by side.

package embed

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mathomhaus/guild/internal/storage"
)

// fakeCorpus adapts a second, independent table set (fake_entities +
// fake_vectors) with prefixed meta keys to the VectorCorpus port. It
// lives next to LoreCorpus so a single *sql.DB can back both corpora
// simultaneously and the adversarial test can prove they do not
// alias.
type fakeCorpus struct{}

var _ VectorCorpus = fakeCorpus{}

func (fakeCorpus) Name() string              { return "fake" }
func (fakeCorpus) VectorTable() string       { return "fake_vectors" }
func (fakeCorpus) EntityTable() string       { return "fake_entities" }
func (fakeCorpus) EntityIDColumn() string    { return "id" }
func (fakeCorpus) VectorStateColumn() string { return "vector_state" }
func (fakeCorpus) ActivePredicate() string   { return "status NOT IN ('archived', 'parked')" }

func (fakeCorpus) SourceText(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	var body string
	err := db.QueryRowContext(ctx,
		`SELECT body FROM fake_entities WHERE id = ?`, entityID,
	).Scan(&body)
	if err != nil {
		return "", err
	}
	return body, nil
}

// MetaKey returns 'fake.'-prefixed keys so fakeCorpus's meta rows do
// not alias with LoreCorpus's unprefixed ones when both share the
// meta table.
func (fakeCorpus) MetaKey(field MetaField) string {
	const prefix = "fake."
	switch field {
	case FieldEmbedderState:
		return prefix + "embedder_state"
	case FieldEmbedderModelID:
		return prefix + "embedder_model_id"
	case FieldEmbedderTokenizerHash:
		return prefix + "embedder_tokenizer_hash"
	case FieldEmbedderRuntimeVersion:
		return prefix + "embedder_runtime_version"
	case FieldEmbedderDim:
		return prefix + "embedder_dim"
	case FieldEmbedderStateReason:
		return prefix + "embedder_state_reason"
	case FieldVectorEpoch:
		return prefix + "vector_epoch"
	case FieldVectorCoverageNum:
		return prefix + "vector_coverage_num"
	case FieldVectorCoverageDen:
		return prefix + "vector_coverage_den"
	case FieldEmbedErrorCount:
		return prefix + "embed_error_count"
	case FieldEmbedLastError:
		return prefix + "embed_last_error"
	case FieldEmbedLastErrorAt:
		return prefix + "embed_last_error_at"
	case FieldEmbedLastOKAt:
		return prefix + "embed_last_ok_at"
	default:
		return ""
	}
}

// createFakeCorpusSchema creates fake_entities + fake_vectors tables
// plus the seeded meta rows the fakeCorpus adapter expects. Runs
// against any *sql.DB that has the base 'meta' table from migration
// 003 already applied. Idempotent (IF NOT EXISTS).
func createFakeCorpusSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS fake_entities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			body TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'current',
			vector_state TEXT NOT NULL DEFAULT 'pending',
			updated_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS fake_vectors (
			entry_id     INTEGER PRIMARY KEY REFERENCES fake_entities(id) ON DELETE CASCADE,
			model_id     TEXT    NOT NULL,
			dim          INTEGER NOT NULL,
			vec          BLOB    NOT NULL,
			encoded_at   INTEGER NOT NULL,
			content_hash TEXT    NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("create fake schema: %v", err)
		}
	}
	// Seed the meta rows under 'fake.'-prefixed keys. Match the
	// production lore seeds the migration 003 installs under
	// unprefixed names.
	seeds := [][2]string{
		{"fake.embedder_model_id", canonModelID},
		{"fake.embedder_tokenizer_hash", ""},
		{"fake.embedder_runtime_version", "test"},
		{"fake.embedder_dim", "384"},
		{"fake.embedder_state", "disabled"},
		{"fake.vector_epoch", "0"},
		{"fake.vector_coverage_num", "0"},
		{"fake.vector_coverage_den", "0"},
		{"fake.embed_error_count", "0"},
	}
	for _, kv := range seeds {
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO meta (key, value) VALUES (?, ?)`,
			kv[0], kv[1],
		); err != nil {
			t.Fatalf("seed fake meta %s: %v", kv[0], err)
		}
	}
}

// seedFakeEntity inserts one fake_entities row with an explicit id and
// returns no value (the id is the caller-provided argument). Used to
// drive scanPending through the fake corpus adapter.
func seedFakeEntity(t *testing.T, db *sql.DB, id int64, body string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO fake_entities (id, body, status, vector_state) VALUES (?, ?, 'current', 'pending')`,
		id, body,
	); err != nil {
		t.Fatalf("seed fake entity %d: %v", id, err)
	}
}

// openDualCorpusDB returns a *sql.DB that has BOTH the lore schema
// (via the storage migrations) AND the fake_* tables applied, plus
// the 'p' project row for lore-side seeds. The two corpora share the
// single meta table; the prefixing contract is what keeps them from
// aliasing.
func openDualCorpusDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openEmbedTestDB(t)
	createFakeCorpusSchema(t, db)
	return db
}

// runCorpusSuite exercises every public operation on a given corpus.
// The same assertions run for LoreCorpus and fakeCorpus, proving
// LSP: no caller of the algorithms can tell which corpus is behind
// the port.
//
// Each corpus gets its own dedicated DB (single-corpus test) so
// isolation is not a confound for the LSP checks. Cross-corpus
// isolation is proven separately in TestCorpus_AdversarialIsolation.
func runCorpusSuite(t *testing.T, corpus VectorCorpus, seed func(t *testing.T, db *sql.DB) []int64) {
	t.Helper()
	ctx := context.Background()

	var db *sql.DB
	switch corpus.(type) {
	case LoreCorpus:
		db = openEmbedTestDB(t)
	case fakeCorpus:
		db = openDualCorpusDB(t)
	default:
		t.Fatalf("runCorpusSuite: unexpected corpus %T", corpus)
	}

	ids := seed(t, db)
	if len(ids) == 0 {
		t.Fatalf("runCorpusSuite: seed returned 0 ids")
	}

	// Set the corpus's embedder_model_id meta row so WriteVector's
	// identity guard agrees with the model_id we pass.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		corpus.MetaKey(FieldEmbedderModelID), canonModelID,
	); err != nil {
		t.Fatalf("%s: set model_id meta: %v", corpus.Name(), err)
	}

	// 1. Backfill every seeded entity.
	res, err := Backfill(ctx, BackfillOptions{
		DB:       db,
		Corpus:   corpus,
		Embedder: NewDeterministicEmbedder(),
		ModelID:  canonModelID,
	})
	if err != nil {
		t.Fatalf("%s: Backfill: %v", corpus.Name(), err)
	}
	if res.Embedded != len(ids) {
		t.Errorf("%s: Backfill Embedded: got %d want %d", corpus.Name(), res.Embedded, len(ids))
	}

	// 2. LoadFromDB reflects the backfilled rows.
	idx := NewIndex(corpus, canonModelID)
	loaded, err := idx.LoadFromDB(ctx, db)
	if err != nil {
		t.Fatalf("%s: LoadFromDB: %v", corpus.Name(), err)
	}
	if loaded != len(ids) {
		t.Errorf("%s: LoadFromDB: loaded %d want %d", corpus.Name(), loaded, len(ids))
	}

	// 3. TopK returns the same number of hits as vectors, descending.
	qvec := Quantize(deterministicUnitVec(42))
	hits, err := idx.TopK(qvec, len(ids))
	if err != nil {
		t.Fatalf("%s: TopK: %v", corpus.Name(), err)
	}
	if len(hits) != len(ids) {
		t.Errorf("%s: TopK returned %d hits, want %d", corpus.Name(), len(hits), len(ids))
	}

	// 4. WriteVector on a NEW entity splices into the index and bumps
	//    epoch + coverage_num. Seed one fresh entity and run
	//    WriteVector through the port.
	var newID int64
	switch corpus.(type) {
	case LoreCorpus:
		mustSeedEntry(t, db, int64(len(ids)+1), "new entry for WriteVector")
		newID = int64(len(ids) + 1)
	case fakeCorpus:
		newID = int64(len(ids) + 1)
		seedFakeEntity(t, db, newID, "new entry for WriteVector")
	}
	preEpoch := idx.Epoch()
	wvRes, err := WriteVector(ctx, db, HotDeps{
		Embedder: NewDeterministicEmbedder(),
		Index:    idx,
		Corpus:   corpus,
		ModelID:  canonModelID,
	}, newID, "new entry for WriteVector")
	if err != nil {
		t.Fatalf("%s: WriteVector: %v", corpus.Name(), err)
	}
	if !wvRes.Written {
		t.Errorf("%s: WriteVector Written=false, want true", corpus.Name())
	}
	if idx.Epoch() <= preEpoch {
		t.Errorf("%s: idx.Epoch did not advance (pre=%d post=%d)", corpus.Name(), preEpoch, idx.Epoch())
	}

	// 5. ReadHealthReport sees the corpus's rows independently.
	report, err := ReadHealthReport(ctx, db, corpus)
	if err != nil {
		t.Fatalf("%s: ReadHealthReport: %v", corpus.Name(), err)
	}
	if report.CoverageNum < int64(len(ids)+1) {
		t.Errorf("%s: CoverageNum: got %d want >=%d",
			corpus.Name(), report.CoverageNum, len(ids)+1)
	}
	if report.VectorEpoch <= 0 {
		t.Errorf("%s: VectorEpoch: got %d want >0", corpus.Name(), report.VectorEpoch)
	}
}

// TestCorpus_LSP_LoreCorpus runs the shared suite against LoreCorpus.
// Proves the lore adapter satisfies every algorithm invariant.
func TestCorpus_LSP_LoreCorpus(t *testing.T) {
	runCorpusSuite(t, LoreCorpus{}, func(t *testing.T, db *sql.DB) []int64 {
		t.Helper()
		ids := make([]int64, 0, 4)
		for i := int64(1); i <= 4; i++ {
			mustSeedEntry(t, db, i, "entry body text "+string(rune('a'+i-1)))
			ids = append(ids, i)
		}
		return ids
	})
}

// TestCorpus_LSP_FakeCorpus runs the shared suite against fakeCorpus.
// Proves adding a brand new corpus is purely additive: no change to
// Index, Backfill, WriteVector, or ReadHealthReport was required to
// make this work.
func TestCorpus_LSP_FakeCorpus(t *testing.T) {
	runCorpusSuite(t, fakeCorpus{}, func(t *testing.T, db *sql.DB) []int64 {
		t.Helper()
		ids := make([]int64, 0, 4)
		for i := int64(1); i <= 4; i++ {
			seedFakeEntity(t, db, i, "fake body text "+string(rune('a'+i-1)))
			ids = append(ids, i)
		}
		return ids
	})
}

// TestCorpus_AdversarialIsolation constructs one *sql.DB backing BOTH
// LoreCorpus AND fakeCorpus, then writes a vector through LoreCorpus
// and asserts:
//
//   - fakeCorpus's Index.Len() is UNCHANGED after the lore write.
//   - fakeCorpus's meta rows ('fake.vector_epoch', 'fake.vector_coverage_num')
//     are UNCHANGED: the lore write did not alias onto them.
//   - The symmetric assertion holds for a fake write: LoreCorpus's
//     Index and meta stay untouched.
//
// A failure in either direction means the MetaKey prefix contract
// leaks: two corpora on one DB would corrupt each other's state.
func TestCorpus_AdversarialIsolation(t *testing.T) {
	ctx := context.Background()
	db := openDualCorpusDB(t)

	// Seed 2 entries in each corpus's entity table.
	mustSeedEntry(t, db, 1, "lore one")
	mustSeedEntry(t, db, 2, "lore two")
	seedFakeEntity(t, db, 101, "fake one")
	seedFakeEntity(t, db, 102, "fake two")

	// Set embedder_model_id on both corpora so WriteVector's guard
	// passes for each independently.
	for _, corpus := range []VectorCorpus{LoreCorpus{}, fakeCorpus{}} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			corpus.MetaKey(FieldEmbedderModelID), canonModelID,
		); err != nil {
			t.Fatalf("%s: set model_id meta: %v", corpus.Name(), err)
		}
	}

	loreIdx := NewIndex(LoreCorpus{}, canonModelID)
	if _, err := loreIdx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("lore LoadFromDB: %v", err)
	}
	fakeIdx := NewIndex(fakeCorpus{}, canonModelID)
	if _, err := fakeIdx.LoadFromDB(ctx, db); err != nil {
		t.Fatalf("fake LoadFromDB: %v", err)
	}

	// Snapshot fake's epoch / coverage_num BEFORE the lore write.
	fakeEpochBefore := readMetaStringDirect(t, db, "fake.vector_epoch")
	fakeCoverageBefore := readMetaStringDirect(t, db, "fake.vector_coverage_num")
	fakeLenBefore := fakeIdx.Len()

	// Write a vector through LoreCorpus. This MUST NOT touch fake.*.
	_, err := WriteVector(ctx, db, HotDeps{
		Embedder: NewDeterministicEmbedder(),
		Index:    loreIdx,
		Corpus:   LoreCorpus{},
		ModelID:  canonModelID,
	}, 1, "lore one body")
	if err != nil {
		t.Fatalf("lore WriteVector: %v", err)
	}

	// fake.* meta rows are UNCHANGED.
	if got := readMetaStringDirect(t, db, "fake.vector_epoch"); got != fakeEpochBefore {
		t.Errorf("fake.vector_epoch polluted by lore write: before=%q after=%q",
			fakeEpochBefore, got)
	}
	if got := readMetaStringDirect(t, db, "fake.vector_coverage_num"); got != fakeCoverageBefore {
		t.Errorf("fake.vector_coverage_num polluted by lore write: before=%q after=%q",
			fakeCoverageBefore, got)
	}
	// fakeIdx.Len is unchanged (we never called CheckAndReload on it,
	// but even if we did, no fake_vectors row was created).
	if got := fakeIdx.Len(); got != fakeLenBefore {
		t.Errorf("fakeIdx.Len polluted by lore write: before=%d after=%d",
			fakeLenBefore, got)
	}

	// Symmetric direction: snapshot lore state, then write through
	// fakeCorpus, and prove lore.* is untouched.
	loreEpochBefore := readMetaStringDirect(t, db, "vector_epoch")
	loreCoverageBefore := readMetaStringDirect(t, db, "vector_coverage_num")
	loreLenBefore := loreIdx.Len()

	_, err = WriteVector(ctx, db, HotDeps{
		Embedder: NewDeterministicEmbedder(),
		Index:    fakeIdx,
		Corpus:   fakeCorpus{},
		ModelID:  canonModelID,
	}, 101, "fake one body")
	if err != nil {
		t.Fatalf("fake WriteVector: %v", err)
	}

	if got := readMetaStringDirect(t, db, "vector_epoch"); got != loreEpochBefore {
		t.Errorf("lore vector_epoch polluted by fake write: before=%q after=%q",
			loreEpochBefore, got)
	}
	if got := readMetaStringDirect(t, db, "vector_coverage_num"); got != loreCoverageBefore {
		t.Errorf("lore vector_coverage_num polluted by fake write: before=%q after=%q",
			loreCoverageBefore, got)
	}
	if got := loreIdx.Len(); got != loreLenBefore {
		t.Errorf("loreIdx.Len polluted by fake write: before=%d after=%d",
			loreLenBefore, got)
	}

	// Additional positive check: each corpus's health report sees
	// only ITS own vectors, not the other's.
	loreReport, err := ReadHealthReport(ctx, db, LoreCorpus{})
	if err != nil {
		t.Fatalf("lore ReadHealthReport: %v", err)
	}
	fakeReport, err := ReadHealthReport(ctx, db, fakeCorpus{})
	if err != nil {
		t.Fatalf("fake ReadHealthReport: %v", err)
	}
	// After exactly one write per corpus, CoverageNum should be 1
	// for each, independently.
	if loreReport.CoverageNum != 1 {
		t.Errorf("lore CoverageNum: got %d want 1", loreReport.CoverageNum)
	}
	if fakeReport.CoverageNum != 1 {
		t.Errorf("fake CoverageNum: got %d want 1", fakeReport.CoverageNum)
	}
}

// readMetaStringDirect reads a meta value by raw key. Kept separate
// from the corpus-aware helpers so isolation tests can check the
// literal stored keys without routing through the adapter we are
// trying to validate.
func readMetaStringDirect(t *testing.T, db *sql.DB, key string) string {
	t.Helper()
	var v sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, key,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("readMetaStringDirect(%q): %v", key, err)
	}
	if !v.Valid {
		return ""
	}
	return v.String
}

// Compile-time smoke check: the storage package is imported even when
// the test file is the only compilation unit exercising it. Keeps
// `go vet` happy if the _ references go unused in a refactor.
var _ = storage.Open
