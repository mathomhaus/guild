package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// trailWriterFunc is the signature for post-claim observability writes.
// The default is writeAcceptTrail; tests can swap it to force failures
// without touching the DB.
type trailWriterFunc func(ctx context.Context, db *sql.DB, projectID, taskID, owner, createdAt string) error

// acceptTrailWriter is the active trail writer. Replaced in tests to
// exercise the "claim committed, trail failed" path without a real DB error.
var acceptTrailWriter trailWriterFunc = writeAcceptTrail

// Accept atomically claims taskID for owner. The atomicity guarantee
// (QUEST-9): two concurrent goroutines calling Accept on the same quest
// must produce exactly one success and one ErrAlreadyClaimed.
//
// How the atomicity works: the UPDATE statement carries the claim-
// eligibility predicate inside its WHERE clause
// (`status='next' AND claimed_by IS NULL`). SQLite evaluates the
// predicate AND the write atomically at the row level because we run in
// a write transaction; the second caller sees the row already claimed
// and rowcount returns 0. We then probe the row to build a useful
// AlreadyClaimedError with the current owner.
//
// Only `next` quests can be claimed (`status='next' AND claimed_by IS
// NULL`), which means a blocked quest can't be claimed until cascade-
// unblock flips it to next. Ordering invariants are preserved.
//
// On success: returns a fully-resolved *Quest with status=in_progress,
// owner=owner, claimed_at=now.
//
// On contention: returns an *AlreadyClaimedError wrapping
// ErrAlreadyClaimed. The caller can errors.Is to branch.
//
// Ownership defaulting: empty owner → "agent".
func Accept(ctx context.Context, db *sql.DB, projectID, taskID, owner string) (*Quest, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: accept: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: accept: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return nil, fmt.Errorf("quest: accept: empty task_id")
	}
	owner = agentOrDefault(owner)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Exists probe first, outside any transaction. This distinguishes
	// "not found" from "already claimed" cleanly. In a race we may read
	// a stale value, but the real atomic check happens in the UPDATE
	// WHERE clause below.
	var existingStatus, existingOwner sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT status, claimed_by FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&existingStatus, &existingOwner)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, taskID)
		}
		return nil, fmt.Errorf("quest: accept: probe existing: %w", err)
	}

	// The CAS (compare-and-swap) UPDATE is the atomic claim. Run it
	// as a single-statement auto-commit write — NOT inside an explicit
	// transaction — so SQLite's write lock is held for the minimum
	// possible time. This is what makes the race test pass: 32
	// goroutines each issue one tiny write, and SQLite's busy_timeout
	// serializes them through the WAL cleanly. With an outer BeginTx
	// that also does follow-up writes (event + checkpoint), we'd hold
	// the write lock across three statements and busy_timeout would
	// trip with high contention.
	//
	// The follow-up writes (claimed event + checkpoint note) are
	// non-critical for the atomicity invariant — they're observability.
	// They go into a small follow-up tx that can SQLITE_BUSY-retry
	// without endangering the primary claim.
	res, err := db.ExecContext(ctx,
		`UPDATE task_status
		 SET status = 'in_progress',
		     claimed_by = ?,
		     claimed_at = ?,
		     updated_at = ?
		 WHERE project_id = ?
		   AND task_id    = ?
		   AND status     = 'next'
		   AND claimed_by IS NULL`,
		owner, now, now, projectID, taskID,
	)
	if err != nil {
		// SQLITE_BUSY during a write means another writer got there
		// first (or our busy_timeout expired). Treat as a contention
		// loss equivalent to "already claimed" — probe current state
		// for the error shape.
		return nil, toAlreadyClaimedOrErr(ctx, db, projectID, taskID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("quest: accept: rows affected: %w", err)
	}
	if rows == 0 {
		// Someone else claimed between our probe and UPDATE (or the
		// row isn't in `next`+unclaimed state). Re-probe for the error.
		var curStatus, curOwner sql.NullString
		_ = db.QueryRowContext(ctx,
			`SELECT status, claimed_by FROM task_status
			 WHERE project_id = ? AND task_id = ?`,
			projectID, taskID,
		).Scan(&curStatus, &curOwner)
		return nil, &AlreadyClaimedError{
			QuestID: taskID,
			Owner:   curOwner.String,
			Status:  Status(curStatus.String),
		}
	}

	// Claim secured. Record the event and auto-checkpoint note in a
	// follow-up tx. These writes don't need to be atomic with the
	// CAS — if they fail the claim still stands (and Scroll output
	// just misses the extra breadcrumbs). Trail failure is observability
	// loss only; it must not convert a committed claim into an API error.
	if err := acceptTrailWriter(ctx, db, projectID, taskID, owner, now); err != nil {
		slog.Warn("quest: accept: trail write failed; claim is durable",
			"quest_id", taskID,
			"error", err,
		)
	}

	return Load(ctx, db, projectID, taskID)
}

// toAlreadyClaimedOrErr maps a SQLITE_BUSY-style UPDATE failure to an
// AlreadyClaimedError where possible. SQLITE_BUSY after the WAL
// busy_timeout expires means *some* concurrent writer held the lock
// long enough that we gave up — which in the Accept path means "we
// lost the race." Surfacing this as ErrAlreadyClaimed (with the
// current owner from a best-effort probe) matches the race invariant
// exactly: exactly one goroutine returns nil, the rest return
// ErrAlreadyClaimed.
//
// For non-BUSY errors we return the raw error wrapped.
func toAlreadyClaimedOrErr(ctx context.Context, db *sql.DB, projectID, taskID string, err error) error {
	if !storage.IsBusyErr(err) {
		return fmt.Errorf("quest: accept: update: %w", err)
	}
	var curStatus, curOwner sql.NullString
	_ = db.QueryRowContext(ctx,
		`SELECT status, claimed_by FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&curStatus, &curOwner)
	return &AlreadyClaimedError{
		QuestID: taskID,
		Owner:   curOwner.String,
		Status:  Status(curStatus.String),
	}
}

// writeAcceptTrail writes the `claimed` event and the auto-checkpoint
// note into a small follow-up transaction. Non-critical for atomicity.
// Retries on SQLITE_BUSY because these writes are still subject to
// contention with other Accept/Clear calls.
func writeAcceptTrail(ctx context.Context, db *sql.DB, projectID, taskID, owner, createdAt string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			lastErr = err
			if !storage.IsBusyErr(err) {
				return fmt.Errorf("quest: accept: begin trail tx: %w", err)
			}
			continue
		}
		if err := emitEvent(ctx, tx, projectID, taskID, EventClaimed, owner, "", createdAt); err != nil {
			_ = tx.Rollback()
			lastErr = err
			if !storage.IsBusyErr(err) {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			projectID, taskID, owner,
			fmt.Sprintf("%saccepted by %s — starting fresh", NotePrefixCheckpoint, owner), createdAt,
		); err != nil {
			_ = tx.Rollback()
			lastErr = err
			if !storage.IsBusyErr(err) {
				return fmt.Errorf("quest: accept: write checkpoint: %w", err)
			}
			continue
		}
		if err := tx.Commit(); err != nil {
			lastErr = err
			if !storage.IsBusyErr(err) {
				return fmt.Errorf("quest: accept: commit trail: %w", err)
			}
			continue
		}
		return nil
	}
	// All retries exhausted. Return the error so the caller (Accept) can
	// log it; the claim is already durable in task_status.
	return fmt.Errorf("quest: accept: trail writes contended out: %w", lastErr)
}
