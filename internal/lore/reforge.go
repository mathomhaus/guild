package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrReforgeSelf is returned when oldID == newID. Surfaced as a typed
// error so callers can match on it with errors.Is.
var ErrReforgeSelf = errors.New("lore: cannot reforge an entry with itself")

// reforgeFaultForTest is a test-only seam that lets the atomicity test
// inject a failure between the UPDATE and the INSERT OR IGNORE so we
// can verify the transaction actually rolls back. Production code never
// sets this; the setter lives in reforge_test.go behind the _test build
// tag convention (same file scope keeps it lexically adjacent for
// audit).
//
// If non-nil and returns a non-nil error, the transaction is rolled
// back and that error is returned — same result shape as if the
// INSERT had failed for a "real" reason.
var reforgeFaultForTest func() error

// Reforge marks oldID as superseded by newID and writes the provenance
// edge newID→oldID with relation=supersedes in one transaction. Either
// both state changes land or neither does — we can't leave the graph in
// a partial state where oldID.status != superseded while the supersedes
// edge exists (or vice-versa).
//
// Cross-project reforge is allowed (both entries are verified to exist
// in SOME project; ProjectID scoping is NOT enforced on reforge). Cross-project
// links are permitted by design; reforge uses the same supersedes relation so it
// inherits that rule for consistency.
//
// When embed is non-nil and Enabled, Reforge dispatches a Tx2 re-embed
// for newID after the Tx1 commits so the successor's vector reflects
// its canonical post-reforge summary. INSERT OR IGNORE on lore_vectors
// means a re-embed of an already-embedded row collapses to a no-op
// plus an epoch bump, which is the intended ADR-003 behavior
// ("lore_reforge: always runs Tx2 re-embed after the row rewrite
// commits"). When embed is nil or disabled, no vector work happens.
func Reforge(ctx context.Context, db *sql.DB, oldID, newID int64, now time.Time, embedDeps *EmbedDeps) error {
	if db == nil {
		return fmt.Errorf("lore: reforge: nil db")
	}
	if oldID == newID {
		return fmt.Errorf("%w: %s", ErrReforgeSelf, formatEntryID(oldID))
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Verify both entries exist before opening a transaction. Keeps the
	// error case cheap; also produces clearer errors ("LORE-99 not
	// found") than a post-rollback one.
	if err := requireEntryExists(ctx, db, oldID); err != nil {
		return err
	}
	if err := requireEntryExists(ctx, db, newID); err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lore: reforge: begin tx: %w", err)
	}
	// Rollback is a no-op after Commit; safe to always-defer.
	defer func() { _ = tx.Rollback() }()

	// Step 1: mark oldID superseded. Touch updated_at so the CLI study
	// view shows when the state actually changed (not just created_at).
	if _, err := tx.ExecContext(ctx,
		`UPDATE entries
		   SET status = 'superseded', updated_at = ?
		 WHERE id = ?`,
		now.Format(time.RFC3339), oldID,
	); err != nil {
		return fmt.Errorf("lore: reforge: update status: %w", err)
	}

	// Test seam: verify the transaction rolls back cleanly when a
	// mid-flight failure occurs. Never nil in production code paths.
	if reforgeFaultForTest != nil {
		if err := reforgeFaultForTest(); err != nil {
			return fmt.Errorf("lore: reforge: injected fault: %w", err)
		}
	}

	// Step 2: write the provenance edge newID → oldID, relation=supersedes.
	// INSERT OR IGNORE so repeated reforges (rare) are idempotent at the
	// edge level — duplicate supersedes edges carry no new information.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO entry_links (from_id, to_id, relation)
		 VALUES (?, ?, 'supersedes')`,
		newID, oldID,
	); err != nil {
		return fmt.Errorf("lore: reforge: insert link: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lore: reforge: commit: %w", err)
	}

	// Tx2 re-embed of the successor entry. Fire-and-forget when async
	// (MCP surface), synchronous otherwise (CLI). A failure here is
	// never surfaced as a Reforge error; the row rewrite is already
	// committed.
	if embedDeps.Enabled() {
		var summary string
		if err := db.QueryRowContext(ctx,
			`SELECT summary FROM entries WHERE id = ?`, newID,
		).Scan(&summary); err != nil {
			// Could not load: skip silently. The caller has already
			// committed the row-level reforge; a missed re-embed will
			// be fixed up on the next init-backfill pass.
			return nil
		}
		embedDeps.runTx2(ctx, db, newID, summary)
	}
	return nil
}
