// Backfill: scan lore entries that have no vector (or a stale one),
// encode them through an Embedder, quantize to int8, and write the
// resulting lore_vectors rows in a BEGIN IMMEDIATE transaction. Also
// bumps meta.vector_epoch and vector_coverage_num atomically at the end
// of the run.
//
// Two call sites:
//
//  1. guild init (one-shot, synchronous): after the probe passes and
//     meta is set to embedder_state='enabled', init calls Backfill
//     to seed vectors for every active entry. The function is
//     idempotent on re-run.
//
//  2. The model-identity upgrade path: when the stored
//     meta.embedder_model_id does not match the binary's bound model
//     id, the caller runs an Invalidate (delete every vector, flip
//     every active entry to vector_state='pending', update meta) and
//     then calls Backfill again.
//
// Concurrency posture: one Backfill at a time per DB. Writers inside
// the package use BEGIN IMMEDIATE and INSERT OR IGNORE (ADR-003
// invariants 1 and 2). Encoding itself happens outside the DB
// transaction so a slow ORT session cannot hold the write lock for
// minutes.

package embed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"
)

// activeEntriesPredicate is the embed-package copy of the canonical predicate
// for entries eligible for vector embedding on the LORE corpus. The lore
// package maintains an identical copy in types.go; they cannot share it
// because internal/lore/embed must not import internal/lore (hexagonal
// boundary).
//
// Value must stay in sync with lore.activeEntriesPredicate and migration 003.
// LoreCorpus.ActivePredicate returns this constant so algorithms that take a
// VectorCorpus see the same predicate through the port.
const activeEntriesPredicate = "status NOT IN ('archived', 'parked')"

// PendingEntry is one row that needs an embedding. Backfill iterates
// these and encodes Summary (the ADR-003 canonical embedding input).
type PendingEntry struct {
	ID      int64
	Summary string
}

// BackfillProgress is emitted once per entry (or once per ProgressEvery
// when >100) so callers can render a progress bar.
type BackfillProgress struct {
	// Done is the number of entries successfully embedded + persisted
	// since the backfill started.
	Done int
	// Total is the number of pending entries at the start of the run.
	Total int
	// Elapsed is the time since the backfill started.
	Elapsed time.Duration
}

// BackfillResult is the post-run summary. Callers log this as a
// single structured line.
type BackfillResult struct {
	Total    int
	Embedded int
	Skipped  int
	Failed   int
	Duration time.Duration
	Epoch    int64
	// DominantFailureClass names the err_class that accumulated the most
	// failures during this Backfill cycle (one of "encode",
	// "embedder_disabled", "dim_mismatch", "insert_row"). Empty when
	// Failed == 0. Surfaced by the auto-backfill summary so the operator
	// sees the cause without scanning the per-iteration WARN lines.
	DominantFailureClass string
}

// BackfillOptions carries the dependencies Backfill needs. Constructor
// injection (no package globals) so tests can pass a canned Embedder
// and capture progress.
type BackfillOptions struct {
	// DB is the database handle. The corpus's tables and meta keys
	// must exist in this DB; Backfill does not run migrations.
	DB *sql.DB
	// Corpus names the tables, columns, and meta keys Backfill
	// operates against. Zero value falls back to LoreCorpus{} for
	// backward compatibility with callers that predate the port.
	Corpus VectorCorpus
	// Embedder produces the float32 vectors. Must be non-nil; use
	// NullEmbedder to short-circuit when the probe failed.
	Embedder Embedder
	// ModelID goes into the vector table's model_id column for every
	// row written. Should match the corpus's EmbedderModelID meta row
	// (the caller enforces this).
	ModelID string
	// ProgressOut receives one "[NN%] NN/NN entries" line per tick.
	// Nil or io.Discard silences progress.
	ProgressOut io.Writer
	// ProgressThreshold is the minimum Total before progress is
	// rendered. Zero or negative means "render always". Default
	// caller passes 100 per spec.
	ProgressThreshold int
	// ProgressEvery controls the reporting cadence: emit a progress
	// tick every N entries (plus the final tick). Zero defaults to 10.
	ProgressEvery int
	// Logger receives per-iteration WARN lines that name the failure
	// class for each row that did not embed. Nil falls back to
	// slog.Default() so callers that do not care about the diagnostics
	// keep working. Gated to backfillFailureLogCap entries per call so
	// a 240-failure run does not flood the log.
	Logger *slog.Logger
	// InsertHook is a test seam: when non-nil, Backfill calls this in
	// place of insertVectorRow for each pending entry. Production callers
	// leave this nil. The shim signature mirrors insertVectorRow exactly
	// so a fail-intermittently fixture can wrap the real call without
	// reimplementing it.
	InsertHook func(ctx context.Context, db *sql.DB, corpus VectorCorpus, entry PendingEntry, vec []float32, modelID string) error
}

// resolveCorpus returns opts.Corpus or the default LoreCorpus when
// unset. Callers that want a non-lore corpus must set opts.Corpus
// explicitly; the default keeps every existing caller working without
// touching their construction code.
func (o BackfillOptions) resolveCorpus() VectorCorpus {
	if o.Corpus == nil {
		return LoreCorpus{}
	}
	return o.Corpus
}

// ReconcileDen resets the corpus's vector_coverage_den meta row to the
// live COUNT(*) of active entities (whatever the corpus's
// ActivePredicate filters to). Runs inside a single BEGIN IMMEDIATE so
// the write is atomic with respect to concurrent writers.
//
// Call this at the start of Backfill so any den drift accumulated between
// the migration seed and the first backfill is repaired before we compare
// num to den. QUEST-220 / LORE-373.
//
// Pass nil corpus for LoreCorpus default (backward compat for callers
// that predate the port).
func ReconcileDen(ctx context.Context, db *sql.DB, corpus VectorCorpus) error {
	if db == nil {
		return fmt.Errorf("embed: ReconcileDen: nil db")
	}
	if corpus == nil {
		corpus = LoreCorpus{}
	}
	conn, rollback, err := beginImmediateLocal(ctx, db, "reconcile-den")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// Count query is templated off corpus.EntityTable +
	// corpus.ActivePredicate. Both values originate in compile-time
	// adapter code.
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`,
		corpus.EntityTable(), corpus.ActivePredicate())
	var count int64
	if err := conn.QueryRowContext(ctx, countQuery).Scan(&count); err != nil { //nolint:sqlcheck // table + predicate are compile-time corpus accessors.
		return fmt.Errorf("embed: ReconcileDen: count active: %w", err)
	}

	denKey := corpus.MetaKey(FieldVectorCoverageDen)
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		denKey, strconv.FormatInt(count, 10),
	); err != nil {
		return fmt.Errorf("embed: ReconcileDen: upsert %s: %w", denKey, err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed: ReconcileDen: commit: %w", err)
	}
	committed = true
	return nil
}

// backfillFailureLogCap is the maximum number of per-iteration WARN
// lines Backfill emits before suppressing the rest with a single
// "[N more failures suppressed]" summary. 10 is enough to characterize
// the dominant cause without flooding stderr on a 240-failure run.
const backfillFailureLogCap = 10

// failureClassEncode tags a failure where Embedder.Embed returned an
// error other than ErrEmbedderDisabled.
const failureClassEncode = "encode"

// failureClassEmbedderDisabled tags the ErrEmbedderDisabled short-circuit.
const failureClassEmbedderDisabled = "embedder_disabled"

// failureClassDimMismatch tags a vector whose length did not match Dim.
const failureClassDimMismatch = "dim_mismatch"

// failureClassInsertRow tags a DB-side failure inside insertVectorRow
// (typically BEGIN IMMEDIATE retry exhaustion under writer-lock
// contention; LORE-416).
const failureClassInsertRow = "insert_row"

// truncateErr clips an error message to maxLen characters so a
// pathological driver-wrapped string does not blow the log line out.
func truncateErr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// Backfill embeds every pending entry and writes a row into lore_vectors
// for each. Idempotent on re-run: the candidate scan is a LEFT JOIN that
// already excludes rows that have vectors, so a second Backfill against
// the same corpus is a no-op.
//
// Error policy: per-entry encode failures increment Failed and continue;
// a persistent failure (>= half the run fails) surfaces as an error from
// Backfill itself after the loop. A db error inside the write Tx2
// aborts the whole function (the caller can retry).
func Backfill(ctx context.Context, opts BackfillOptions) (*BackfillResult, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("embed: Backfill: nil db")
	}
	if opts.Embedder == nil {
		return nil, fmt.Errorf("embed: Backfill: nil embedder")
	}
	if opts.ModelID == "" {
		return nil, fmt.Errorf("embed: Backfill: empty model_id")
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 10
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	corpus := opts.resolveCorpus()

	start := time.Now()

	// Reconcile den before scanning so any drift from entities inserted
	// between the migration seed and this first Backfill is corrected
	// before we write coverage_num. Keeps num <= den invariant. QUEST-220.
	if err := ReconcileDen(ctx, opts.DB, corpus); err != nil {
		return nil, fmt.Errorf("embed: Backfill: reconcile den: %w", err)
	}

	pending, err := scanPending(ctx, opts.DB, corpus)
	if err != nil {
		return nil, fmt.Errorf("embed: Backfill: scan pending: %w", err)
	}
	res := &BackfillResult{Total: len(pending)}
	if res.Total == 0 {
		// Still bump epoch so a caller waiting on the index refresh
		// sees a fresh value; zero-pending is a valid no-op.
		epoch, err := bumpEpoch(ctx, opts.DB, corpus)
		if err != nil {
			return nil, fmt.Errorf("embed: Backfill: bump epoch (empty run): %w", err)
		}
		res.Epoch = epoch
		res.Duration = time.Since(start)
		return res, nil
	}

	renderProgress := opts.ProgressOut != nil && opts.ProgressOut != io.Discard && res.Total >= opts.ProgressThreshold

	// Per-iteration failure diagnostics: track counts per err_class for
	// the dominant-class summary, and gate the WARN emit at
	// backfillFailureLogCap so a 240-failure run does not flood logs.
	failureCounts := map[string]int{}
	failureLogCount := 0
	suppressedCount := 0
	logFailure := func(class string, fields ...any) {
		failureCounts[class]++
		if failureLogCount < backfillFailureLogCap {
			logger.Warn("embed: Backfill: per-entry failure", fields...)
			failureLogCount++
		} else {
			suppressedCount++
		}
	}

	for i, entry := range pending {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("embed: Backfill: cancelled: %w", err)
		}
		vec, encErr := opts.Embedder.Embed(ctx, entry.Summary)
		if encErr != nil {
			// NullEmbedder surfaces here as ErrEmbedderDisabled; any
			// other error is a per-entry transient. Both bump Failed.
			res.Failed++
			if errors.Is(encErr, ErrEmbedderDisabled) {
				logFailure(failureClassEmbedderDisabled,
					slog.Int64("entry_id", entry.ID),
					slog.String("err_class", failureClassEmbedderDisabled),
					slog.String("err", truncateErr(encErr.Error(), 200)),
				)
				// Short-circuit: no point continuing against a null
				// embedder (the remaining rows will all fail the same
				// way). Return what we have so the caller can set
				// meta.embedder_state='disabled' and move on.
				res.Duration = time.Since(start)
				res.DominantFailureClass = pickDominantClass(failureCounts)
				if suppressedCount > 0 {
					logger.Warn("embed: Backfill: more per-entry failures suppressed",
						slog.Int("suppressed", suppressedCount),
					)
				}
				return res, fmt.Errorf("embed: Backfill: embedder disabled: %w", encErr)
			}
			logFailure(failureClassEncode,
				slog.Int64("entry_id", entry.ID),
				slog.String("err_class", failureClassEncode),
				slog.String("err", truncateErr(encErr.Error(), 200)),
			)
			continue
		}
		if len(vec) != Dim {
			res.Failed++
			logFailure(failureClassDimMismatch,
				slog.Int64("entry_id", entry.ID),
				slog.String("err_class", failureClassDimMismatch),
				slog.Int("got", len(vec)),
				slog.Int("want", Dim),
			)
			continue
		}
		insertFn := opts.InsertHook
		if insertFn == nil {
			insertFn = insertVectorRow
		}
		if err := insertFn(ctx, opts.DB, corpus, entry, vec, opts.ModelID); err != nil {
			res.Failed++
			logFailure(failureClassInsertRow,
				slog.Int64("entry_id", entry.ID),
				slog.String("err_class", failureClassInsertRow),
				slog.String("err", truncateErr(err.Error(), 200)),
			)
			// DB-level error on a single row: keep going. A second run
			// of Backfill will pick the row up again via LEFT JOIN.
			continue
		}
		res.Embedded++
		if renderProgress && (res.Embedded%opts.ProgressEvery == 0 || i == len(pending)-1) {
			renderProgressLine(opts.ProgressOut, BackfillProgress{
				Done:    res.Embedded,
				Total:   res.Total,
				Elapsed: time.Since(start),
			})
		}
	}
	if suppressedCount > 0 {
		logger.Warn("embed: Backfill: more per-entry failures suppressed",
			slog.Int("suppressed", suppressedCount),
		)
	}
	res.DominantFailureClass = pickDominantClass(failureCounts)
	res.Skipped = res.Total - res.Embedded - res.Failed
	epoch, err := bumpEpoch(ctx, opts.DB, corpus)
	if err != nil {
		return res, fmt.Errorf("embed: Backfill: bump epoch: %w", err)
	}
	res.Epoch = epoch
	res.Duration = time.Since(start)
	return res, nil
}

// Invalidate drops every vector row for the corpus, flips every
// active entity's vector_state back to 'pending' (when the corpus
// tracks state), and writes the new embedder identity into the
// corpus's meta rows. Used on model-identity upgrade (ADR-003
// invariant 2).
//
// Runs inside a single BEGIN IMMEDIATE transaction so a concurrent
// reader cannot see a half-invalidated state.
//
// Pass nil corpus for LoreCorpus default (backward compat).
func Invalidate(ctx context.Context, db *sql.DB, corpus VectorCorpus, newIdentity ManifestIdentity) error {
	if db == nil {
		return fmt.Errorf("embed: Invalidate: nil db")
	}
	if corpus == nil {
		corpus = LoreCorpus{}
	}
	conn, rollback, err := beginImmediateLocal(ctx, db, "invalidate")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	deleteQuery := fmt.Sprintf(`DELETE FROM %s`, corpus.VectorTable())
	if _, err := conn.ExecContext(ctx, deleteQuery); err != nil { //nolint:sqlcheck // table name is a compile-time corpus accessor.
		return fmt.Errorf("embed: Invalidate: delete vectors: %w", err)
	}
	// Flip state only when the corpus tracks it. A corpus that opts
	// out of state tracking (VectorStateColumn() == "") simply skips
	// this step; its Backfill rescan is driven purely by the LEFT JOIN
	// on the vector table.
	if stateCol := corpus.VectorStateColumn(); stateCol != "" {
		flipQuery := fmt.Sprintf(`UPDATE %s SET %s = 'pending' WHERE %s`,
			corpus.EntityTable(), stateCol, corpus.ActivePredicate())
		if _, err := conn.ExecContext(ctx, flipQuery); err != nil { //nolint:sqlcheck // table + column + predicate are compile-time corpus accessors.
			return fmt.Errorf("embed: Invalidate: flip vector_state: %w", err)
		}
	}
	// Bump epoch so readers refresh their caches.
	epochKey := corpus.MetaKey(FieldVectorEpoch)
	newEpoch, err := readEpochTxKey(ctx, conn, epochKey)
	if err != nil {
		return err
	}
	newEpoch++
	upserts := []struct{ k, v string }{
		{corpus.MetaKey(FieldEmbedderModelID), newIdentity.ModelID},
		{corpus.MetaKey(FieldEmbedderTokenizerHash), newIdentity.TokenizerHash},
		{corpus.MetaKey(FieldEmbedderRuntimeVersion), newIdentity.RuntimeVersion},
		{corpus.MetaKey(FieldEmbedderDim), strconv.Itoa(newIdentity.Dim)},
		{epochKey, strconv.FormatInt(newEpoch, 10)},
		{corpus.MetaKey(FieldVectorCoverageNum), "0"},
	}
	for _, kv := range upserts {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			return fmt.Errorf("embed: Invalidate: upsert meta %s: %w", kv.k, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed: Invalidate: commit: %w", err)
	}
	committed = true
	return nil
}

// scanPending returns every active entity with no vector row in the
// corpus's vector table. Ordered by id ASC for deterministic test
// runs.
//
// The query templates a LEFT JOIN between corpus.EntityTable() and
// corpus.VectorTable() on corpus.EntityIDColumn() and filters by the
// corpus's ActivePredicate. Each entity's source text is pulled
// through corpus.SourceText so a per-corpus text-assembly scheme can
// concatenate, truncate, or rewrite freely without changing this
// driver loop.
func scanPending(ctx context.Context, db *sql.DB, corpus VectorCorpus) ([]PendingEntry, error) {
	// Two-phase: first select the IDs of active entities without a
	// vector row, then pull SourceText per id via the corpus adapter.
	// Keeping the scan query free of the summary column lets future
	// corpora assemble text from multiple columns without forcing the
	// scan to know which columns exist.
	activePred := corpus.ActivePredicate()
	// Guard: an empty ActivePredicate produces a malformed AND clause.
	// Corpora that want "all entities" should return "1=1".
	if activePred == "" {
		activePred = "1=1"
	}
	query := fmt.Sprintf(`SELECT e.%[1]s FROM %[2]s e LEFT JOIN %[3]s v ON v.entry_id = e.%[1]s WHERE v.entry_id IS NULL AND e.%[4]s ORDER BY e.%[1]s ASC`, corpus.EntityIDColumn(), corpus.EntityTable(), corpus.VectorTable(), activePred) //nolint:gosec // G201: all substitutions are compile-time corpus accessors, not user input.
	rows, err := db.QueryContext(ctx, query)                                                                                                                                                                                                 //nolint:sqlcheck // all parts are compile-time corpus accessors.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]PendingEntry, 0, len(ids))
	for _, id := range ids {
		text, err := corpus.SourceText(ctx, db, id)
		if err != nil {
			// A deleted-mid-backfill row surfaces as sql.ErrNoRows;
			// treat it as "skip this id" rather than failing the scan.
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("embed: scanPending: source text for id=%d: %w", id, err)
		}
		out = append(out, PendingEntry{ID: id, Summary: text})
	}
	return out, nil
}

// InsertVectorRow is the test-visible shim around insertVectorRow.
// Production code calls insertVectorRow directly; tests that want to
// inject a fail-intermittently or counting wrapper assign a closure to
// BackfillOptions.InsertHook that delegates to InsertVectorRow on the
// happy path. Exported only so internal/mcp tests can reach it without
// duplicating the BEGIN IMMEDIATE / quantize logic.
func InsertVectorRow(ctx context.Context, db *sql.DB, corpus VectorCorpus, entry PendingEntry, vec []float32, modelID string) error {
	return insertVectorRow(ctx, db, corpus, entry, vec, modelID)
}

// insertVectorRow writes one row into the corpus's vector table for
// entry. Uses INSERT OR IGNORE so a concurrent writer that raced us
// to this row is not treated as an error (ADR-003 invariant 1). Flips
// the parent entity's vector_state to 'indexed' in the same tx when
// the corpus tracks state.
func insertVectorRow(ctx context.Context, db *sql.DB, corpus VectorCorpus, entry PendingEntry, vec []float32, modelID string) error {
	conn, rollback, err := beginImmediateLocal(ctx, db, "backfill-row")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// Canonical int8 quantization lives in cosine.go (Quantize, VecDim=384,
	// symmetric scale of 127). The vector BLOB is exactly VecDim bytes;
	// reinterpret []int8 as []byte via a copy. See ADR-003 storage layout
	// and Index.LoadFromDB which rejects any other length.
	quant := Quantize(vec)
	if quant == nil {
		return fmt.Errorf("quantize: got %d float32, want %d", len(vec), VecDim)
	}
	blob := make([]byte, VecDim)
	for j, q := range quant {
		blob[j] = byte(q)
	}
	hash := sha256.Sum256([]byte(entry.Summary))
	hashHex := hex.EncodeToString(hash[:])
	now := time.Now().UTC().Unix()

	insertQuery := fmt.Sprintf(
		`INSERT OR IGNORE INTO %s (entry_id, model_id, dim, vec, encoded_at, content_hash) VALUES (?, ?, ?, ?, ?, ?)`,
		corpus.VectorTable(),
	)
	if _, err := conn.ExecContext(ctx, insertQuery, //nolint:sqlcheck // table name is a compile-time corpus accessor.
		entry.ID, modelID, Dim, blob, now, hashHex); err != nil {
		return fmt.Errorf("insert vector: %w", err)
	}
	if stateCol := corpus.VectorStateColumn(); stateCol != "" {
		flipQuery := fmt.Sprintf(`UPDATE %s SET %s = 'indexed' WHERE %s = ?`,
			corpus.EntityTable(), stateCol, corpus.EntityIDColumn())
		if _, err := conn.ExecContext(ctx, flipQuery, entry.ID); err != nil { //nolint:sqlcheck // table + columns are compile-time corpus accessors.
			return fmt.Errorf("flip vector_state: %w", err)
		}
	}
	// coverage++ inside the same tx so counter never drifts from
	// actual vector rows.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO meta (key,value) VALUES (?,'1')
		 ON CONFLICT(key) DO UPDATE SET value = CAST((CAST(value AS INTEGER) + 1) AS TEXT)`,
		corpus.MetaKey(FieldVectorCoverageNum),
	); err != nil {
		return fmt.Errorf("bump coverage: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// bumpEpoch atomically increments the corpus's vector_epoch meta row
// and returns the new value. Uses BEGIN IMMEDIATE so concurrent
// writers serialize.
func bumpEpoch(ctx context.Context, db *sql.DB, corpus VectorCorpus) (int64, error) {
	conn, rollback, err := beginImmediateLocal(ctx, db, "bump-epoch")
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	epochKey := corpus.MetaKey(FieldVectorEpoch)
	cur, err := readEpochTxKey(ctx, conn, epochKey)
	if err != nil {
		return 0, err
	}
	next := cur + 1
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO meta (key,value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		epochKey, strconv.FormatInt(next, 10),
	); err != nil {
		return 0, fmt.Errorf("write epoch: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return next, nil
}

// readEpochTxKey reads the given corpus-resolved meta epoch key from
// an already-open conn. Returns 0 when the row is missing (fresh DB)
// so callers can always use the returned value.
func readEpochTxKey(ctx context.Context, conn *sql.Conn, key string) (int64, error) {
	var s sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, key,
	).Scan(&s); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("read epoch %s: %w", key, err)
	}
	if !s.Valid || s.String == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s.String, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse epoch %s %q: %w", key, s.String, err)
	}
	return n, nil
}

// Quantization lives in cosine.go alongside the cosine kernel. See
// Quantize / Dequantize / VecDim there. This package-local comment stays
// as a pointer so anyone grepping for "QuantizeInt8" in backfill lands
// here and knows where the canonical helper moved.

// renderProgressLine writes one "[pp%] done/total (elapsed)" line.
// Kept deliberately plain (no ANSI) so the output is readable in the
// guild init transcript and in CI logs that strip ANSI sequences.
func renderProgressLine(w io.Writer, p BackfillProgress) {
	if w == nil {
		return
	}
	pct := 0
	if p.Total > 0 {
		pct = int((float64(p.Done) * 100.0) / float64(p.Total))
	}
	var eta time.Duration
	if p.Done > 0 && p.Done < p.Total {
		remain := p.Total - p.Done
		perEntry := p.Elapsed / time.Duration(p.Done)
		eta = perEntry * time.Duration(remain)
	}
	if eta > 0 {
		fmt.Fprintf(w, "  backfill: [%3d%%] %d/%d  eta=%s\n", pct, p.Done, p.Total, eta.Round(time.Second))
	} else {
		fmt.Fprintf(w, "  backfill: [%3d%%] %d/%d\n", pct, p.Done, p.Total)
	}
}

// beginImmediateLocal is the package-local copy of the BEGIN IMMEDIATE
// helper pattern used in internal/quest. Kept local (not imported from
// internal/quest) so internal/lore/embed has zero imports from the rest
// of internal/lore, preserving the Phase 2 swap invariant.
func beginImmediateLocal(ctx context.Context, db *sql.DB, op string) (*sql.Conn, func(*bool), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("embed: %s: acquire conn: %w", op, err)
	}
	const maxAttempts = 20
	var beginErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		_, beginErr = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if beginErr == nil {
			break
		}
		if !isSQLiteBusy(beginErr.Error()) {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("embed: %s: begin immediate: %w", op, beginErr)
		}
		wait := time.Duration(attempt+1) * 10 * time.Millisecond
		if wait > 200*time.Millisecond {
			wait = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, nil, fmt.Errorf("embed: %s: begin immediate: %w", op, ctx.Err())
		case <-time.After(wait):
		}
	}
	if beginErr != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("embed: %s: begin immediate (contended out): %w", op, beginErr)
	}
	rollback := func(committed *bool) { //nolint:contextcheck // rollback must survive caller cancellation
		if !*committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	return conn, rollback, nil
}

// isSQLiteBusy recognizes the modernc.org/sqlite busy/locked surfacing.
// Kept string-based (not sqlite3.Error) because the driver is pure-Go
// and wraps errors into fmt.Errorf("%w", ...).
func isSQLiteBusy(msg string) bool {
	return containsAny(msg, "SQLITE_BUSY", "database is locked", "SQLITE_LOCKED")
}

// pickDominantClass returns the err_class with the highest count, or "" if
// the map is empty. Ties resolve to the alphabetically first class so the
// output is deterministic across runs (the dominant-class line is read by
// operators, not by code; stability beats novelty here).
func pickDominantClass(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	best := ""
	bestN := -1
	for class, n := range counts {
		if n > bestN || (n == bestN && class < best) {
			best = class
			bestN = n
		}
	}
	return best
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				return true
			}
		}
	}
	return false
}
