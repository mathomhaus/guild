package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file is the quest-side body of the daemon lease reaper (ADR-005
// Phase 3, "Why a daemon" item 3). The daemon owns WHEN to sweep (its own
// ticker, in internal/daemon/reaper.go); ReapExpiredLeases is WHAT one
// sweep does. Keeping the orchestration here means internal/daemon never
// imports internal/quest, the same leaf split the heartbeat tick already
// uses for renewals.
//
// HARD INVARIANT (no-daemon byte-identical): the reaper scans task_leases
// ONLY. A claim accepted without the daemon writes no lease row (see
// lease.go), so it is invisible to this sweep by construction and can never
// be falsely forfeited. The no-daemon path keeps today's freedom to hold a
// claim across days untouched. No leases, no false forfeits.

// SessionLiveFunc reports whether a session is still registered (a live
// unix-socket connection) in the daemon. The reaper consults it before
// forfeiting: a lease whose expires_at lapsed but whose session is still
// connected is a renewal hiccup (a missed heartbeat tick under load), not a
// crash, so the reaper leaves it for the heartbeat tick to renew rather than
// forfeiting a live agent's work. A nil predicate (or one always returning
// false) treats every expired lease as a crash candidate, the conservative
// default the host overrides with the real registry lookup.
type SessionLiveFunc func(sessionID string) bool

// ReapResult tallies one sweep for the daemon's logs and the
// daemon-status/presence readout. All counts are per-sweep, not cumulative.
type ReapResult struct {
	// Scanned is how many expired lease rows the sweep examined.
	Scanned int
	// Forfeited is how many zombie claims the sweep auto-released back to
	// the board (status flipped in_progress -> next, with a released event
	// and a [released] note).
	Forfeited int
	// SkippedLive is how many expired leases the sweep left alone because
	// their session was still registered (a renewal hiccup, not a crash).
	SkippedLive int
	// OrphansCleared is how many expired lease rows the sweep deleted
	// WITHOUT forfeiting, because the underlying claim was already
	// re-assigned, fulfilled, forfeited, or otherwise no longer the
	// in_progress claim the lease was written for.
	OrphansCleared int
}

// ReapExpiredLeases walks every expired lease and forfeits the zombie claim
// behind it, the daemon reaper's per-sweep body. For each expired lease it
// applies three guards before acting:
//
//	(a) session liveness: if isLive reports the lease's session is still
//	    registered, this is a missed heartbeat under load, not a crash; the
//	    lease is left for the heartbeat tick to renew and nothing is touched.
//	(b) claim identity: the task_status row must still be in_progress AND
//	    claimed_by must still equal the lease holder. A claim that was
//	    re-assigned, fulfilled, or forfeited since the lease was written no
//	    longer matches; its stale lease row is deleted (orphan cleanup) but
//	    the live claim is never disturbed.
//	(c) Forfeit's own BEGIN IMMEDIATE status gating closes the residual race
//	    between guard (b) and the forfeit write: if another actor flips the
//	    status in that window, Forfeit returns AlreadyNext or ErrAlreadyDone
//	    and the reaper treats both as a no-op, then still drops the lease.
//	    Two overlapping sweeps therefore produce exactly one release.
//
// A successful forfeit and an orphan both delete the lease row so the next
// sweep does not re-examine it. A per-lease error (the forfeit or a probe
// failing on SQLITE_BUSY) is collected and returned joined; the sweep
// continues to the next lease so one stuck row never blocks the rest. The
// daemon logs the error and retries on the next tick. now is injected for
// deterministic expiry math (the daemon passes its clock's Now()).
func ReapExpiredLeases(ctx context.Context, db *sql.DB, now time.Time, isLive SessionLiveFunc) (ReapResult, error) {
	var res ReapResult
	if db == nil {
		return res, fmt.Errorf("quest: reap leases: nil db")
	}

	expired, err := ExpiredLeases(ctx, db, now)
	if err != nil {
		return res, err
	}
	res.Scanned = len(expired)

	var errs []error
	for _, l := range expired {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}

		// Guard (a): a still-registered session is a renewal hiccup, not a
		// crash. Leave the lease for the heartbeat tick; do not forfeit.
		if isLive != nil && isLive(l.SessionID) {
			res.SkippedLive++
			continue
		}

		// Guard (b): the claim must still be the in_progress claim this
		// lease was written for. Anything else is an orphan: drop the lease
		// row and move on without touching task_status.
		match, err := claimStillHeldBy(ctx, db, l.ProjectID, l.TaskID, l.Holder)
		if err != nil {
			errs = append(errs, fmt.Errorf("quest: reap %s/%s: probe claim: %w", l.ProjectID, l.TaskID, err))
			continue
		}
		if !match {
			if err := DeleteLease(ctx, db, l.ProjectID, l.TaskID); err != nil {
				errs = append(errs, fmt.Errorf("quest: reap %s/%s: delete orphan lease: %w", l.ProjectID, l.TaskID, err))
				continue
			}
			res.OrphansCleared++
			continue
		}

		// Guard (c): Forfeit's BEGIN IMMEDIATE re-checks status under a write
		// lock, so a status flip racing this call is handled inside Forfeit.
		// AlreadyNext (raced to next) and ErrAlreadyDone (raced to done) are
		// both no-ops for the reaper; ErrNotFound means the row vanished. In
		// every terminal case the lease row is now stale and gets deleted.
		note := reapNote(l)
		fr, err := Forfeit(ctx, db, l.ProjectID, l.TaskID, note)
		switch {
		case err == nil && fr != nil && !fr.AlreadyNext:
			res.Forfeited++
		case err == nil:
			// AlreadyNext: a concurrent actor (or the other overlapping
			// sweep) already released it. No second release.
			res.OrphansCleared++
		case errors.Is(err, ErrAlreadyDone), errors.Is(err, ErrNotFound):
			// Done or gone: nothing to release; just clear the lease below.
			res.OrphansCleared++
		default:
			errs = append(errs, fmt.Errorf("quest: reap %s/%s: forfeit: %w", l.ProjectID, l.TaskID, err))
			continue
		}

		if err := DeleteLease(ctx, db, l.ProjectID, l.TaskID); err != nil {
			errs = append(errs, fmt.Errorf("quest: reap %s/%s: delete lease: %w", l.ProjectID, l.TaskID, err))
		}
	}

	return res, errors.Join(errs...)
}

// claimStillHeldBy reports whether task (projectID, taskID) is still
// status=in_progress AND claimed_by equals holder, the identity guard the
// reaper checks before forfeiting. A missing row, a non-in_progress status,
// or a different holder all return false (the lease is an orphan). Comparison
// trims and is case-insensitive on the holder so a stored "agent" still
// matches a lease holder of "Agent"; the holder is always set by the same
// AcquireLease path the claim's Accept used, so the values agree in practice.
func claimStillHeldBy(ctx context.Context, db *sql.DB, projectID, taskID, holder string) (bool, error) {
	var status, claimedBy sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT status, claimed_by FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&status, &claimedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if Status(status.String) != StatusInProgress {
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(claimedBy.String), strings.TrimSpace(holder)), nil
}

// reapNote composes the [released]-prefixed reason Forfeit stores on an
// auto-forfeit, so a scroll of the requeued quest shows WHY it returned to
// the board and WHEN the agent went silent. Forfeit prepends "[released] ".
func reapNote(l Lease) string {
	since := strings.TrimSpace(l.HeartbeatAt)
	if since == "" {
		since = strings.TrimSpace(l.ExpiresAt)
	}
	return fmt.Sprintf("lease expired: agent session %s stopped responding (last heartbeat %s); claim auto-released by the guild daemon", l.SessionID, since)
}
