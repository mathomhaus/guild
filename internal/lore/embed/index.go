package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
)

// Typed errors for caller-side branching. Embed callers (Phase 1.4
// MCP/CLI wiring in QUEST-212) compare via errors.Is rather than
// inspecting error text, so these sentinels are the public contract.
var (
	// ErrIndexStale signals that the in-process index has not been
	// loaded yet (LoadFromDB has not run, or returned an error and
	// left the index empty). Callers can retry LoadFromDB or fall
	// through to BM25-only retrieval.
	ErrIndexStale = errors.New("embed/index: index is stale; LoadFromDB required")

	// ErrModelMismatch signals that LoadFromDB found vector rows
	// whose model_id does not match the meta.embedder_model_id
	// canonical identity. Per ADR-003 vector-versioning, the index
	// skips those rows; this error is returned only when strict
	// enforcement is requested (reserved for future use by callers
	// that want to assert full coverage before enabling RRF).
	ErrModelMismatch = errors.New("embed/index: stored model_id != meta.embedder_model_id")

	// ErrQueryShape signals that TopK received a query vector of the
	// wrong length. Guards against the caller forgetting to pass
	// through the embedder's Dim and handing the index a truncated
	// slice.
	ErrQueryShape = errors.New("embed/index: query vector has wrong dim")
)

// ScoredEntry is one hit from TopK: the entry's primary key and its
// int32 dot-product score under the Quantize convention. TopK returns
// hits in descending score order.
//
// The score is the raw int32 dot product (see cosineInt8). Callers
// that need the cosine in [-1, 1] can divide by quantScale*quantScale
// or call CosineFloat on the original vectors.
type ScoredEntry struct {
	EntryID int64
	Score   int32
}

// Index is a per-process, in-memory vector index over a single
// corpus's vector table. It holds parallel slices of int8 vectors and
// entity IDs plus a cached epoch; all access is guarded by a single
// sync.RWMutex per ADR-003 invariant 3.
//
// Lifecycle (caller's responsibility):
//
//  1. NewIndex to construct.
//  2. LoadFromDB at server startup (not lazy-on-first-query).
//  3. CheckAndReload(ctx, db) at the top of every appraise call; it
//     is a single indexed meta-table read and a no-op on the common
//     path where no writer has advanced the epoch.
//  4. Splice from the Tx2 background goroutine after a successful
//     vector write, so the writer process does not pay a full reload.
//  5. TopK for queries.
//
// Hexagonal boundary: Index knows about int8 BLOBs, entity IDs, and
// epochs. It knows nothing about Embedder, query strings, RRF, or the
// caller's concept of a "project." The caller owns the *sql.DB and
// picks a VectorCorpus adapter plus a bound model_id at construction
// time; the adapter tells the Index which table, column, and meta key
// to consult for this corpus.
type Index struct {
	// bindMu serializes full-index mutations (Load/Reload + Splice).
	// A sync.RWMutex is sufficient for the production access pattern:
	// many concurrent readers (TopK, CheckAndReload's epoch read) and
	// a low-rate writer path (one Splice per inscribe).
	bindMu sync.RWMutex

	// corpus names the tables, columns, and meta keys this index
	// operates against. Set at construction and never mutated.
	// Algorithms template SQL off the accessors; values originate in
	// compile-time adapter code so fmt.Sprintf is safe here.
	corpus VectorCorpus

	// modelID is the canonical identity string this index trusts. Set
	// at construction and never mutated. Rows whose vector table's
	// model_id column disagrees with this value are skipped during
	// LoadFromDB.
	modelID string

	// vectors is a tight []int8Vec slice; len(vectors[i]) == VecDim.
	// Kept separate from entries so the hot cosine scan touches only
	// the int8 BLOBs and never indirects through a struct.
	vectors [][]int8

	// entries[i] is the lore_entries.id for vectors[i]. Parallel
	// slice: grown together with vectors, shrunk together.
	entries []int64

	// byEntry maps entry_id -> slot index in vectors/entries. Keeps
	// Splice O(1) instead of O(n) when updating an existing entry
	// (lore_reforge or lore_update with summary change).
	byEntry map[int64]int

	// cachedEpoch is the meta.vector_epoch value observed at the last
	// successful LoadFromDB or Splice. CheckAndReload compares this
	// against the freshly read epoch under the read lock to decide
	// whether to reload.
	cachedEpoch int64

	// loaded flips true after the first successful LoadFromDB. A
	// zero-row load still counts as loaded (legitimate empty corpus).
	loaded bool

	// logger receives diagnostic lines (dropped rows, epoch jumps).
	// Defaults to slog.Default if the caller does not override.
	logger *slog.Logger
}

// Option tunes a new Index. Functional options keep the public
// constructor surface one argument even as the struct grows.
type Option func(*Index)

// WithLogger attaches a custom slog.Logger. Without this the index
// uses slog.Default(), fine for production where the caller has
// already configured a project-wide handler.
func WithLogger(l *slog.Logger) Option {
	return func(i *Index) { i.logger = l }
}

// NewIndex constructs an empty Index bound to the given corpus and
// model_id. The returned index is valid but reports loaded=false
// until LoadFromDB runs; TopK against an unloaded index returns
// ErrIndexStale.
//
// corpus names the tables and meta keys the index operates against.
// LoreCorpus{} is the lore adapter; future corpora plug in without
// any change to this constructor.
//
// modelID must equal the binary's embedded manifest identity (see
// ADR-003 "Vector versioning"). At the time of writing, the canonical
// value is "bge-small-en-v1.5-int8-cls"; pass the string from the
// caller rather than hard-coding it here so the index remains
// decoupled from the model lifecycle.
func NewIndex(corpus VectorCorpus, modelID string, opts ...Option) *Index {
	idx := &Index{
		corpus:  corpus,
		modelID: modelID,
		byEntry: make(map[int64]int),
	}
	for _, o := range opts {
		o(idx)
	}
	if idx.logger == nil {
		idx.logger = slog.Default()
	}
	return idx
}

// Len returns the number of vectors currently indexed. Test helper;
// real callers should not use this to gate retrieval (use the
// meta.vector_coverage_* pair per ADR-003).
func (i *Index) Len() int {
	i.bindMu.RLock()
	defer i.bindMu.RUnlock()
	return len(i.vectors)
}

// Epoch returns the last epoch the index observed. Primarily for
// tests and diagnostics; callers can call CheckAndReload instead of
// reading the epoch directly.
func (i *Index) Epoch() int64 {
	i.bindMu.RLock()
	defer i.bindMu.RUnlock()
	return i.cachedEpoch
}

// ModelID returns the canonical model_id this index was constructed
// with. Useful when the caller wants to log the binding or assert
// it matches a refreshed embedder.
func (i *Index) ModelID() string {
	return i.modelID
}

// LoadFromDB reads every lore_vectors row whose model_id matches this
// index's bound model_id and populates the parallel slices in one
// pass. Also refreshes cachedEpoch from meta.vector_epoch. Safe to
// call multiple times (each call fully replaces the in-memory state
// under the write lock).
//
// Returns the number of rows loaded. Rows whose BLOB is not exactly
// VecDim bytes are skipped with a single-line warn log; the rest of
// the corpus still loads. This matches ADR-003 invariant 2 philosophy:
// Tx2 refuses to write a mismatched vector, so an out-of-shape blob
// indicates corruption and the index prefers partial coverage plus a
// log line over a hard load failure.
func (i *Index) LoadFromDB(ctx context.Context, db *sql.DB) (int, error) {
	if db == nil {
		return 0, errors.New("embed/index: LoadFromDB: nil *sql.DB")
	}
	if i.corpus == nil {
		return 0, errors.New("embed/index: LoadFromDB: nil corpus")
	}

	// Read the canonical model_id from meta first. If it disagrees
	// with the bound model_id, the whole corpus is mismatched and
	// the caller needs to know; return ErrModelMismatch without
	// clobbering the existing index. The meta key is resolved via
	// the corpus adapter so a non-lore corpus looks up its own row.
	var metaModelID string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`,
		i.corpus.MetaKey(FieldEmbedderModelID),
	).Scan(&metaModelID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh DB before the seed ran; treat as empty but still
		// refresh cachedEpoch below. No warning: this is the normal
		// state of a DB mid-migration.
	case err != nil:
		return 0, fmt.Errorf("embed/index: read meta embedder_model_id: %w", err)
	default:
		if metaModelID != i.modelID {
			i.logger.Warn("embed/index: model identity mismatch; refusing load",
				"corpus", i.corpus.Name(),
				"bound_model_id", i.modelID,
				"meta_model_id", metaModelID,
			)
			return 0, ErrModelMismatch
		}
	}

	// Read the epoch BEFORE scanning rows so it is a lower bound for
	// the snapshot: every commit visible to the SELECT below happened
	// at or after this epoch. Reading it after the scan inverts that
	// bound and lets a concurrent writer's commit land between the two
	// reads, stamping a pre-commit row snapshot with a post-commit
	// epoch; CheckAndReload then trusts the stale snapshot forever.
	epoch, err := readEpoch(ctx, db, i.corpus.MetaKey(FieldVectorEpoch))
	if err != nil {
		return 0, err
	}

	// Stream the corpus's vector table in one SELECT. entry_id is the
	// PK so the default ordering is by id; that is fine for parallel-
	// slice layout and gives tests a deterministic load order.
	//
	// fmt.Sprintf is safe here: the table name originates in compile-
	// time adapter code (LoreCorpus{}.VectorTable returns a literal),
	// never from user input. Query parameters still flow through the
	// driver's placeholder substitution. gosec G201 fires on any SQL
	// Sprintf so we suppress it locally.
	query := fmt.Sprintf(`SELECT entry_id, vec FROM %s WHERE model_id = ? ORDER BY entry_id`, i.corpus.VectorTable()) //nolint:gosec // G201: table name is a compile-time constant from the corpus adapter, not user input.
	rows, err := db.QueryContext(ctx, query, i.modelID)                                                               //nolint:sqlcheck // query templated from compile-time corpus.VectorTable(), not user input; ? placeholders preserved.
	if err != nil {
		return 0, fmt.Errorf("embed/index: SELECT %s: %w", i.corpus.VectorTable(), err)
	}
	defer func() { _ = rows.Close() }()

	// Build the new slices under no lock; swap into place once the
	// whole read is done. Keeps the write-lock hold time to O(1)
	// rather than O(n) of SQLite round trips.
	var (
		newVecs    [][]int8
		newEntries []int64
		newByID    = make(map[int64]int)
		skipped    int
	)
	for rows.Next() {
		var (
			id   int64
			blob []byte
		)
		if err := rows.Scan(&id, &blob); err != nil {
			return 0, fmt.Errorf("embed/index: scan row: %w", err)
		}
		if len(blob) != VecDim {
			i.logger.Warn("embed/index: skipping malformed vector row",
				"entry_id", id,
				"blob_len", len(blob),
				"want_len", VecDim,
			)
			skipped++
			continue
		}
		// Reinterpret the []byte BLOB as []int8. Go guarantees int8
		// and byte (uint8) share representation so a single allocation
		// + copy is correct and cheap. Do not alias the driver's
		// buffer; modernc.org/sqlite reuses per-row slices.
		vec := make([]int8, VecDim)
		for j := 0; j < VecDim; j++ {
			vec[j] = int8(blob[j])
		}
		slot := len(newVecs)
		newVecs = append(newVecs, vec)
		newEntries = append(newEntries, id)
		newByID[id] = slot
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("embed/index: iterate %s: %w", i.corpus.VectorTable(), err)
	}

	i.bindMu.Lock()
	if i.loaded && epoch < i.cachedEpoch {
		// A concurrent Splice advanced the index past this snapshot
		// (its writer committed after our epoch read). Installing the
		// snapshot would silently drop that newer vector. Discard;
		// the in-memory state is already at least as fresh as the DB
		// state this load observed.
		cached := i.cachedEpoch
		i.bindMu.Unlock()
		i.logger.Debug("embed/index: discarded stale load snapshot",
			"snapshot_epoch", epoch,
			"cached_epoch", cached,
			"rows", len(newVecs),
		)
		return len(newVecs), nil
	}
	i.vectors = newVecs
	i.entries = newEntries
	i.byEntry = newByID
	i.cachedEpoch = epoch
	i.loaded = true
	i.bindMu.Unlock()

	if skipped > 0 {
		i.logger.Warn("embed/index: load completed with malformed rows skipped",
			"loaded", len(newVecs),
			"skipped", skipped,
			"epoch", epoch,
		)
	}
	return len(newVecs), nil
}

// CheckAndReload reads meta.vector_epoch; if it exceeds the cached
// epoch, reloads the index. The common path is a single indexed
// integer read and an atomic compare, so callers can safely invoke
// this at the top of every appraise without measurable overhead.
//
// Returns true if a reload happened. Errors from the underlying read
// are returned verbatim; callers that want BM25-only fallback on
// reload failure should handle the error rather than panic.
func (i *Index) CheckAndReload(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, errors.New("embed/index: CheckAndReload: nil *sql.DB")
	}
	if i.corpus == nil {
		return false, errors.New("embed/index: CheckAndReload: nil corpus")
	}

	epoch, err := readEpoch(ctx, db, i.corpus.MetaKey(FieldVectorEpoch))
	if err != nil {
		return false, err
	}

	i.bindMu.RLock()
	cached := i.cachedEpoch
	loaded := i.loaded
	i.bindMu.RUnlock()

	if loaded && epoch == cached {
		return false, nil
	}

	// Epoch advanced (or we have not loaded yet); take the slow path.
	if _, err := i.LoadFromDB(ctx, db); err != nil {
		return false, err
	}
	return true, nil
}

// Splice updates or inserts a single vector under the write lock and
// bumps cachedEpoch to newEpoch. Used by the Tx2 writer path so the
// writer process does not have to round-trip through LoadFromDB after
// a successful inscribe.
//
// If entryID already has a slot, its vector is replaced in place. The
// Quantize contract is the caller's responsibility; Splice verifies
// the length is VecDim and errors otherwise. newEpoch must be >= the
// current cachedEpoch; a strictly smaller value returns an error to
// catch caller bugs (e.g. passing the pre-increment epoch).
func (i *Index) Splice(entryID int64, vec []int8, newEpoch int64) error {
	if len(vec) != VecDim {
		return fmt.Errorf("embed/index: Splice: vec len %d != %d", len(vec), VecDim)
	}

	// Defensive copy: callers may reuse a buffer for the next embed.
	// Copying here is cheap (384 bytes) and keeps the index's internal
	// state fully owned.
	stored := make([]int8, VecDim)
	copy(stored, vec)

	i.bindMu.Lock()
	defer i.bindMu.Unlock()

	if newEpoch < i.cachedEpoch {
		return fmt.Errorf("embed/index: Splice: newEpoch %d < cached %d", newEpoch, i.cachedEpoch)
	}

	if slot, ok := i.byEntry[entryID]; ok {
		i.vectors[slot] = stored
	} else {
		slot := len(i.vectors)
		i.vectors = append(i.vectors, stored)
		i.entries = append(i.entries, entryID)
		i.byEntry[entryID] = slot
	}
	i.cachedEpoch = newEpoch
	// A Splice on an unloaded index flips loaded=true: the caller is
	// asserting the row is canonical, so subsequent CheckAndReload
	// calls can trust the cached epoch rather than forcing a reload.
	i.loaded = true
	return nil
}

// TopK returns up to k highest-scoring entries by cosine similarity
// against qvec, descending. Caller passes the int8-quantized query
// vector (use Quantize on the embedder's float32 output).
//
// Rationale for int8-to-int8 scoring over float32 dequantize: a
// single pass at 10k x 384 bytes on an Apple M3 Pro runs ~0.5 ms; the
// float32 alternative is ~5x slower and produces the same ranking
// under the quantization convention. ADR-003's "tight []int8Vec"
// guidance is explicit about this.
//
// Returns an empty slice (not nil) when the index is empty but
// loaded. Returns ErrIndexStale when LoadFromDB has not run. Returns
// ErrQueryShape on bad qvec length.
func (i *Index) TopK(qvec []int8, k int) ([]ScoredEntry, error) {
	if len(qvec) != VecDim {
		return nil, ErrQueryShape
	}
	if k <= 0 {
		return []ScoredEntry{}, nil
	}

	i.bindMu.RLock()
	defer i.bindMu.RUnlock()

	if !i.loaded {
		return nil, ErrIndexStale
	}
	n := len(i.vectors)
	if n == 0 {
		return []ScoredEntry{}, nil
	}
	if k > n {
		k = n
	}

	// Full linear scan. At 10k x 384 bytes we are comfortably under
	// the L1 working-set fit for a modern laptop CPU; the scan is
	// bandwidth-bound, not compute-bound, so a heap-based top-k
	// prune does not materially beat "score all then partial sort"
	// until the corpus is substantially larger. Revisit if ever.
	scores := make([]ScoredEntry, n)
	for j := 0; j < n; j++ {
		scores[j] = ScoredEntry{
			EntryID: i.entries[j],
			Score:   cosineInt8(qvec, i.vectors[j]),
		}
	}

	// Partial sort: we only need the top k. sort.Slice with a
	// descending comparator is the simplest correct code; the
	// performance difference vs. a heap at k=60, n=10k is below noise
	// on the target hardware.
	sort.Slice(scores, func(a, b int) bool {
		if scores[a].Score != scores[b].Score {
			return scores[a].Score > scores[b].Score
		}
		// Deterministic tiebreak on entry_id ascending so identical
		// scores produce identical output across runs (important for
		// the RRF merge downstream).
		return scores[a].EntryID < scores[b].EntryID
	})
	return scores[:k], nil
}

// readEpoch fetches the corpus's vector_epoch meta value as an int64.
// A missing row yields zero, matching the seed value in migration
// 003. Parse errors are returned so callers see a corrupt meta row
// instead of silently treating it as zero.
//
// key is the corpus-resolved meta key (LoreCorpus uses 'vector_epoch';
// future corpora may use 'quest.vector_epoch' etc.).
func readEpoch(ctx context.Context, db *sql.DB, key string) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, key,
	).Scan(&s)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("embed/index: read meta %s: %w", key, err)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("embed/index: parse meta %s %q: %w", key, s, err)
	}
	return n, nil
}
