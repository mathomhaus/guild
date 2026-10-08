package embed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Tx2 encodes outside the database lock, then checks model identity, source
// text and eligibility inside BEGIN IMMEDIATE. It replaces incompatible or
// content-stale rows and advances epoch only when a vector changes. Diagnostic
// counters are recomputed from eligible rows rather than incremented blindly.

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
	// Written is true when a row was inserted or repaired. A false result
	// means unchanged content, a concurrent source edit, inactive/deleted
	// entity, a disabled embedder, or an identity mismatch.
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

// WriteVector encodes and conditionally persists one current source. A failed
// encode retains an existing vector. Concurrent edits cannot be overwritten by
// a delayed encoder. The returned epoch describes the committed row change.
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
		if err := bumpEmbedErrorCount(ctx, db, corpus, "invalid_vector"); err != nil {
			logger.Warn("embed/hot: record invalid vector failed", "err", err)
		}
		return WriteVectorResult{}, fmt.Errorf("embed/hot: quantize rejected nonfinite or zero vector")
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

	result, err := writeEncodedTx(ctx, conn, corpus, entryID, summary, qvec, deps.ModelID)
	if err != nil {
		return WriteVectorResult{}, err
	}
	epoch := result.Epoch
	inserted := int64(0)
	if result.Written {
		inserted = 1
	}
	contentHash := result.ContentHash

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return WriteVectorResult{}, fmt.Errorf("embed/hot: commit: %w", err)
	}
	committed = true

	// Splice is the caller's responsibility: a race-free splice
	// against the in-process index requires the caller's mutex, not
	// the embed package's. Splice + epoch bump must happen under the
	// same lock window so the cached epoch never lags the splice.
	if deps.Index != nil && result.Written {
		if err := deps.Index.Splice(entryID, qvec, epoch); err != nil {
			// Splice only fails on a malformed vector (out-of-order
			// epochs are resolved internally per slot). The DB row is
			// canonical either way. Log and continue.
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
	for _, kv := range []struct{ k, v string }{{corpus.MetaKey(FieldEmbedLastError), reason}, {corpus.MetaKey(FieldEmbedLastErrorAt), time.Now().UTC().Format(time.RFC3339)}} {
		if _, err := conn.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv.k, kv.v); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed/hot: bump embed_error_count commit: %w", err)
	}
	committed = true
	return nil
}
