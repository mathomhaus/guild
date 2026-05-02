package embed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Tx2 invariants (ADR-003 "Concurrency invariants"):
//
//  1. All vector writes use INSERT OR IGNORE on lore_vectors. entry_id
//     is the PK, so a racing CLI inscribe + backfill pass that both
//     encode the same row produce one row, never a constraint error.
//  2. Model identity is re-read from meta at the start of Tx2 and
//     compared against the embedder's bound model_id. A mismatch
//     aborts cleanly (no user-visible error), increments
//     meta.embed_error_count, and logs once with reason=binary_outdated.
//  3. meta.vector_epoch and meta.vector_coverage_num are bumped
//     atomically inside the same BEGIN IMMEDIATE that writes the row.
//
// This file is the only production writer to lore_vectors. Callers in
// internal/lore (inscribe/update/reforge) construct a HotDeps and call
// WriteVector from their own post-commit path. Backfill (QUEST-210) is
// expected to reuse WriteVector once that quest lands; the entry
// points are identical.

// HotDeps is the set of dependencies WriteVector needs. Constructed by
// the caller (lore package today, cmd/guild tomorrow) and passed
// through as a value so the Tx2 helper has zero global state.
//
// Every field is required except Logger (nil defaults to slog.Default)
// and Corpus (nil defaults to LoreCorpus{} for backward compat).
//
// ModelID is the embedder's bound model_id, which must equal the
// corpus's EmbedderModelID meta row at WriteVector time. Mismatch
// triggers the graceful abort path documented in invariant 2.
type HotDeps struct {
	// Embedder encodes the summary text into a float32 vector. The
	// hot path quantizes to int8 via Quantize. Must be non-nil (the
	// caller checks this before dispatching; WriteVector double-checks).
	Embedder Embedder

	// Index is the in-process vector index to splice into after the
	// row commits. May be nil on the CLI surface (ADR-003 "Dataflow:
	// CLI surface") where short-lived processes do not maintain an
	// index; the Tx2 write still succeeds and other processes pick up
	// the row via their own epoch check. nil Index skips the splice
	// and leaves cross-process reload as the only notification path.
	Index *Index

	// Corpus names the tables, columns, and meta keys WriteVector
	// operates against. Zero value falls back to LoreCorpus{} so
	// callers that predate the port keep working.
	Corpus VectorCorpus

	// ModelID is the canonical identity the binary ships with. At Tx2
	// open we SELECT the corpus's EmbedderModelID meta row and compare;
	// a mismatch means a newer binary has re-seeded the DB and this
	// older binary should not write.
	ModelID string

	// Logger receives one structured line per Tx2 outcome: success
	// (debug), model_id mismatch (warn), and unexpected SQL failures
	// (error). nil defaults to slog.Default.
	Logger *slog.Logger
}

// resolveCorpus returns deps.Corpus or the default LoreCorpus when
// unset. Kept as a method so every internal use site spells the
// default identically.
func (d HotDeps) resolveCorpus() VectorCorpus {
	if d.Corpus == nil {
		return LoreCorpus{}
	}
	return d.Corpus
}

// WriteVectorResult reports what WriteVector did. Callers use this to
// decide whether to splice into a local index and what to log/emit.
// Empty struct on any of the "graceful skip" outcomes (model mismatch,
// embedder disabled) so the caller can detect those via Written=false.
type WriteVectorResult struct {
	// Written is true only when the INSERT OR IGNORE caused a new row
	// to appear. A false-written result with no error means either
	// (a) the row already existed (coexisting writer won the race), or
	// (b) the model_id guard aborted the Tx2 cleanly.
	Written bool

	// Epoch is the meta.vector_epoch value after WriteVector. On
	// Written=true this is the post-bump value; on Written=false it
	// is the current value (unchanged). Callers can pass this to
	// Index.Splice so the local splice + meta bump stay consistent.
	Epoch int64

	// Vec is the int8-quantized vector that was stored. The caller
	// uses it to Splice into its in-process index without re-reading
	// the BLOB from SQLite. Empty when Written=false.
	Vec []int8

	// ContentHash is the SHA-256 hex digest of the summary text that
	// was embedded. The caller compares against this on a subsequent
	// update to detect a no-op edit and skip re-embedding.
	ContentHash string
}

// ErrEmbedderNotProvided signals that WriteVector was called with a
// nil Embedder. The caller is expected to skip the vector-write path
// entirely when no embedder is configured (Windows, flag off) rather
// than invoking WriteVector and hitting this error.
var ErrEmbedderNotProvided = errors.New("embed/hot: nil Embedder")

// WriteVector runs the full Tx2 sequence for a single entry:
//
//  1. Encode summary via the embedder (outside the transaction).
//  2. Open BEGIN IMMEDIATE on a dedicated connection.
//  3. SELECT meta.embedder_model_id; abort if != deps.ModelID.
//  4. INSERT OR IGNORE lore_vectors row.
//  5. UPDATE entries SET vector_state='indexed' WHERE id = ? AND
//     vector_state != 'indexed' (so a concurrent winner's state stands).
//  6. Atomic meta bumps: vector_epoch = vector_epoch + 1,
//     vector_coverage_num = vector_coverage_num + 1 ONLY when the
//     INSERT actually produced a new row.
//  7. COMMIT.
//
// On any SQL error after BEGIN, Tx2 rolls back and returns the error
// wrapped with %w so callers can errors.Is against driver sentinels.
// On a model-id mismatch, Tx2 bumps meta.embed_error_count (in its own
// tiny follow-up exec), logs once, and returns nil with Written=false.
// This matches ADR-003's "no user-visible error on binary_outdated".
func WriteVector(ctx context.Context, db *sql.DB, deps HotDeps, entryID int64, summary string) (WriteVectorResult, error) {
	if db == nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: WriteVector: nil *sql.DB")
	}
	if deps.Embedder == nil {
		return WriteVectorResult{}, ErrEmbedderNotProvided
	}
	if strings.TrimSpace(deps.ModelID) == "" {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: WriteVector: empty ModelID")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	corpus := deps.resolveCorpus()

	// 1. Encode outside the transaction. Embedder.Embed is ~0.9 ms
	//    on the hot path; keeping it outside the lock window keeps
	//    BEGIN IMMEDIATE hold-time to the SQL round trips only.
	startEmbed := time.Now()
	fvec, err := deps.Embedder.Embed(ctx, summary)
	embedDur := time.Since(startEmbed)
	if err != nil {
		// Graceful skip on disabled embedder. The caller has already
		// gated on embedder_state=='enabled', so reaching here with
		// ErrEmbedderDisabled means a race with a concurrent flip;
		// log once and return Written=false without incrementing
		// embed_error_count (not a real failure).
		if errors.Is(err, ErrEmbedderDisabled) {
			logger.Debug("embed/hot: embedder disabled mid-write",
				"entry_id", entryID,
			)
			return WriteVectorResult{}, nil
		}
		if err := bumpEmbedErrorCount(ctx, db, corpus, "embed_failed"); err != nil {
			logger.Error("embed/hot: bump embed_error_count failed",
				"entry_id", entryID,
				"bump_err", err,
			)
		}
		return WriteVectorResult{}, fmt.Errorf("embed/hot: encode: %w", err)
	}
	if len(fvec) != VecDim {
		if err := bumpEmbedErrorCount(ctx, db, corpus, "bad_dim"); err != nil {
			logger.Error("embed/hot: bump embed_error_count failed",
				"entry_id", entryID,
				"bump_err", err,
			)
		}
		return WriteVectorResult{}, fmt.Errorf("embed/hot: encode: got %d dims, want %d", len(fvec), VecDim)
	}
	qvec := Quantize(fvec)
	if qvec == nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: quantize returned nil (len(fvec)=%d)", len(fvec))
	}

	// 2. Take a dedicated conn and BEGIN IMMEDIATE. Shares
	//    beginImmediateLocal with backfill.go so the init-backfill
	//    and hot (inscribe/update/reforge) paths use an identical
	//    concurrency primitive: one place to fix bugs, one place to
	//    tune retries. Hexagonal boundary: no imports from lore or
	//    quest.
	conn, rollback, err := beginImmediateLocal(ctx, db, "embed/hot: WriteVector")
	if err != nil {
		return WriteVectorResult{}, err
	}
	defer func() { _ = conn.Close() }()
	committed := false
	defer rollback(&committed)

	// 3. Model identity guard. A mismatch is a graceful abort. Meta
	//    key is corpus-resolved so a non-lore corpus uses its own
	//    EmbedderModelID row and does not alias with lore's.
	var metaModelID string
	err = conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`,
		corpus.MetaKey(FieldEmbedderModelID),
	).Scan(&metaModelID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh DB with no seeded model identity. Treat as mismatch
		// so we do not accidentally populate a DB that the next init
		// will re-seed under a different identity.
		metaModelID = ""
	case err != nil:
		return WriteVectorResult{}, fmt.Errorf("embed/hot: read meta embedder_model_id: %w", err)
	}
	if metaModelID != deps.ModelID {
		// Graceful skip: rollback the open transaction, release the
		// connection back to the pool, and then perform the error
		// counter bump in its own BEGIN IMMEDIATE. Rolling back
		// explicitly (rather than relying on the deferred rollback
		// + defer-ordered Close) avoids the pool returning our own
		// pinned conn with a still-open tx to bumpEmbedErrorCount.
		if _, rbErr := conn.ExecContext(ctx, "ROLLBACK"); rbErr != nil {
			logger.Warn("embed/hot: rollback after model_id mismatch failed",
				"entry_id", entryID,
				"err", rbErr,
			)
		}
		// Mark committed so the deferred rollback does not re-issue.
		committed = true
		_ = conn.Close()
		logger.Warn("embed/hot: model_id mismatch; skipping Tx2",
			"entry_id", entryID,
			"bound_model_id", deps.ModelID,
			"meta_model_id", metaModelID,
			"reason", "binary_outdated",
		)
		if err := bumpEmbedErrorCount(ctx, db, corpus, "binary_outdated"); err != nil {
			logger.Error("embed/hot: bump embed_error_count failed",
				"entry_id", entryID,
				"bump_err", err,
			)
		}
		return WriteVectorResult{}, nil
	}

	// 4. INSERT OR IGNORE. entry_id is the PK; a concurrent backfill
	//    writer that raced us leaves our INSERT as a no-op and our
	//    counter bump as a skip (detected via RowsAffected).
	ch := sha256.Sum256([]byte(summary))
	contentHash := hex.EncodeToString(ch[:])
	nowUnix := time.Now().Unix()
	blob := make([]byte, VecDim)
	for i, v := range qvec {
		blob[i] = byte(v)
	}
	insertQuery := fmt.Sprintf(`INSERT OR IGNORE INTO %s (entry_id, model_id, dim, vec, encoded_at, content_hash) VALUES (?, ?, ?, ?, ?, ?)`, corpus.VectorTable()) //nolint:gosec // G201: table name is a compile-time corpus accessor, not user input.
	res, err := conn.ExecContext(ctx, insertQuery,                                                                                                                  //nolint:sqlcheck // table name is a compile-time corpus accessor.
		entryID, deps.ModelID, VecDim, blob, nowUnix, contentHash)
	if err != nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: insert lore_vectors: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: rows affected: %w", err)
	}

	// If the INSERT was a no-op (row already existed), another
	// writer populated the vector first. Still make the entry's
	// state reflect that a vector is present and advance the epoch
	// so other readers refresh their index (in case the winning
	// writer was another process whose splice we missed). Skip the
	// coverage_num bump in that case.
	// If inserted, bump coverage_num atomically. Epoch always bumps
	// when a new vector appears; on no-op we still bump only when we
	// changed the entry's state (vector_state flip).
	epochKey := corpus.MetaKey(FieldVectorEpoch)
	coverageNumKey := corpus.MetaKey(FieldVectorCoverageNum)
	if inserted > 0 {
		// UPDATE vector_state='indexed' when the corpus tracks state.
		// Harmless if already indexed (rare: self-race).
		if stateCol := corpus.VectorStateColumn(); stateCol != "" {
			flipQuery := fmt.Sprintf(`UPDATE %s SET %s = 'indexed', updated_at = updated_at WHERE %s = ?`,
				corpus.EntityTable(), stateCol, corpus.EntityIDColumn())
			if _, err := conn.ExecContext(ctx, flipQuery, entryID); err != nil { //nolint:sqlcheck // table + columns are compile-time corpus accessors.
				return WriteVectorResult{}, fmt.Errorf("embed/hot: update entries vector_state: %w", err)
			}
		}
		// Atomic counter bumps. The "X = X + 1" form is evaluated
		// inside SQLite under the BEGIN IMMEDIATE, so the read and
		// write happen without interleaving any other writer. Keys are
		// corpus-resolved so two corpora cannot alias on a shared
		// meta row.
		if _, err := conn.ExecContext(ctx, `
			UPDATE meta
			   SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
			 WHERE key IN (?, ?)
		`, epochKey, coverageNumKey); err != nil {
			return WriteVectorResult{}, fmt.Errorf("embed/hot: bump epoch and coverage_num: %w", err)
		}
	} else {
		// INSERT lost the race; the row exists under some other
		// writer's content_hash. We still want the cached epoch to
		// advance so other readers reload and observe the new vector.
		// Do NOT bump coverage_num (the other writer already did).
		if _, err := conn.ExecContext(ctx, `
			UPDATE meta
			   SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
			 WHERE key = ?
		`, epochKey); err != nil {
			return WriteVectorResult{}, fmt.Errorf("embed/hot: bump epoch (noop insert): %w", err)
		}
	}

	// Read back the new epoch so the caller can pass it to Splice
	// without an additional round trip. This is still inside the
	// BEGIN IMMEDIATE so the value reflects our own write.
	var epochStr string
	if err := conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, epochKey,
	).Scan(&epochStr); err != nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: read epoch: %w", err)
	}
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: parse epoch %q: %w", epochStr, err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: commit: %w", err)
	}
	committed = true

	// Splice is the caller's responsibility: a race-free splice
	// against the in-process index requires the caller's mutex, not
	// the embed package's. Splice + epoch bump must happen under the
	// same lock window so the cached epoch never lags the splice.
	if deps.Index != nil {
		if err := deps.Index.Splice(entryID, qvec, epoch); err != nil {
			// A Splice failure is non-fatal: other readers will pick
			// up the row via CheckAndReload. Log and continue.
			logger.Warn("embed/hot: index splice failed after commit",
				"entry_id", entryID,
				"err", err,
			)
		}
	}
	logger.Debug("embed/hot: wrote vector",
		"entry_id", entryID,
		"inserted", inserted > 0,
		"epoch", epoch,
		"embed_ms", embedDur.Milliseconds(),
	)
	return WriteVectorResult{
		Written:     inserted > 0,
		Epoch:       epoch,
		Vec:         qvec,
		ContentHash: contentHash,
	}, nil
}

// ContentHash returns the canonical SHA-256 hex digest used by the
// vector-versioning path (lore_vectors.content_hash). Exposed so
// internal/lore.Update can compare a new summary against the stored
// hash and decide whether to re-embed without duplicating the hash
// function here.
func ContentHash(summary string) string {
	sum := sha256.Sum256([]byte(summary))
	return hex.EncodeToString(sum[:])
}

// bumpEmbedErrorCount is the helper for the two "graceful abort" paths
// (model mismatch, embedder failure). Opens its own BEGIN IMMEDIATE
// because the main Tx2 has either not yet opened or has already
// rolled back. The key is resolved through the corpus so a non-lore
// corpus increments its own counter instead of aliasing on lore's.
func bumpEmbedErrorCount(ctx context.Context, db *sql.DB, corpus VectorCorpus, reason string) error {
	conn, rollback, err := beginImmediateLocal(ctx, db, "embed/hot: bump embed_error_count")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	committed := false
	defer rollback(&committed)
	if _, err := conn.ExecContext(ctx, `
		UPDATE meta
		   SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		 WHERE key = ?
	`, corpus.MetaKey(FieldEmbedErrorCount)); err != nil {
		return fmt.Errorf("embed/hot: bump embed_error_count (%s): %w", reason, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed/hot: bump embed_error_count commit: %w", err)
	}
	committed = true
	return nil
}
