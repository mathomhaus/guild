// Backfill repairs missing, stale, and incompatible vectors without resetting
// a corpus. Encoding happens outside the writer lock; persistence rechecks
// canonical source text, model identity, and entity eligibility under that lock.
// Every changed row advances the epoch in the same transaction. Failed encodes
// retain existing rows; a later startup can retry them.

package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
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

	var modelID string
	if err := conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, corpus.MetaKey(FieldEmbedderModelID)).Scan(&modelID); err != nil {
		return err
	}
	if err := reconcileCoverageTx(ctx, conn, corpus, modelID); err != nil {
		return err
	}
	query := fmt.Sprintf(`DELETE FROM %s WHERE NOT EXISTS(SELECT 1 FROM %s e WHERE e.%s=entry_id)`, corpus.VectorTable(), corpus.EntityTable(), corpus.EntityIDColumn()) //nolint:gosec // compile-time corpus accessors
	deleted, err := conn.ExecContext(ctx, query)                                                                                                                         //nolint:sqlcheck // compile-time corpus accessors
	if err != nil {
		return err
	}
	count, err := deleted.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		epoch, err := readEpochTxKey(ctx, conn, corpus.MetaKey(FieldVectorEpoch))
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, corpus.MetaKey(FieldVectorEpoch), fmt.Sprint(epoch+1)); err != nil {
			return err
		}
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

// Backfill repairs eligible sources whose vector is missing, incompatible, or
// content-stale. Individual failures are recorded and the remaining rows run;
// cancellation and scan failures return the work completed so far.
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
	// Migration seeds new corpora with an empty identity. Bind that empty
	// slot atomically; never replace another writer's nonempty model.
	conn, rollback, err := beginImmediateLocal(ctx, opts.DB, "backfill-bind")
	if err != nil {
		return nil, err
	}
	committed := false
	if _, err = conn.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE value=''`, corpus.MetaKey(FieldEmbedderModelID), opts.ModelID); err == nil {
		_, err = conn.ExecContext(ctx, "COMMIT")
		committed = err == nil
	}
	rollback(&committed)
	_ = conn.Close()
	if err != nil {
		return nil, err
	}

	// Reconcile den before scanning so any drift from entities inserted
	// between the migration seed and this first Backfill is corrected
	// before we write coverage_num. Keeps num <= den invariant. QUEST-220.
	if err := ReconcileDen(ctx, opts.DB, corpus); err != nil {
		return nil, fmt.Errorf("embed: Backfill: reconcile den: %w", err)
	}

	pending, err := scanRepairPending(ctx, opts.DB, corpus, opts.ModelID)
	if err != nil {
		return nil, fmt.Errorf("embed: Backfill: scan pending: %w", err)
	}
	res := &BackfillResult{Total: len(pending)}

	renderProgress := opts.ProgressOut != nil && opts.ProgressOut != io.Discard && res.Total >= opts.ProgressThreshold

	// Per-iteration failure diagnostics: track counts per err_class for
	// the dominant-class summary, and gate the WARN emit at
	// backfillFailureLogCap so a 240-failure run does not flood logs.
	failureCounts := map[string]int{}
	failureLogCount := 0
	suppressedCount := 0
	logFailure := func(class string, fields ...any) {
		failureCounts[class]++
		if err := bumpEmbedErrorCount(ctx, opts.DB, corpus, class); err != nil {
			logger.Warn("embed: backfill error metadata write failed", "err", err)
		}
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
			if errors.Is(err, errVectorUnchanged) {
				res.Skipped++
				continue
			}
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
	epoch, err := readEpoch(ctx, opts.DB, corpus.MetaKey(FieldVectorEpoch))
	if err != nil {
		return res, err
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

var errVectorUnchanged = errors.New("embed: vector unchanged or source changed")

func scanPending(ctx context.Context, db *sql.DB, corpus VectorCorpus) ([]PendingEntry, error) {
	var model string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, corpus.MetaKey(FieldEmbedderModelID)).Scan(&model); err != nil {
		return nil, err
	}
	return scanRepairPending(ctx, db, corpus, model)
}

func scanRepairPending(ctx context.Context, db *sql.DB, corpus VectorCorpus, modelID string) ([]PendingEntry, error) {
	coverage, err := ReadCoverage(ctx, db, corpus, modelID, nil)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY %s`, corpus.EntityIDColumn(), corpus.EntityTable(), corpus.ActivePredicate(), corpus.EntityIDColumn()) //nolint:gosec // compile-time corpus accessors
	rows, err := db.QueryContext(ctx, query)                                                                                                                         //nolint:sqlcheck // compile-time corpus accessors
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !coverage.FreshIDs[id] {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var out []PendingEntry
	for _, id := range ids {
		text, err := corpus.SourceText(ctx, db, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
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

// insertVectorRow checks source freshness and writes a replacement atomically.
// An unchanged row or an edit that won the race is reported as a skip.
func insertVectorRow(ctx context.Context, db *sql.DB, corpus VectorCorpus, entry PendingEntry, vec []float32, modelID string) error {
	conn, rollback, err := beginImmediateLocal(ctx, db, "backfill-row")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	quant := Quantize(vec)
	if quant == nil {
		return fmt.Errorf("quantize: invalid shape, nonfinite or zero vector (dim=%d)", len(vec))
	}
	result, err := writeEncodedTx(ctx, conn, corpus, entry.ID, entry.Summary, quant, modelID)
	if err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	if !result.Written {
		return errVectorUnchanged
	}
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
		if !storage.IsBusy(beginErr) {
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
