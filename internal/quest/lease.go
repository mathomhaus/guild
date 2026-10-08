package quest

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/command"
)

// Quest leases are the liveness layer for daemon-mediated claims
// (ADR-005 Part 1, "Why a daemon" item 3). A bare claim on task_status
// (claimed_by, claimed_at) carries no notion of whether the claimant is
// still alive, so a crashed agent's in_progress quest rots as a zombie.
// A daemon that sees every session holds a lease with a heartbeat; when
// the heartbeat stops the lease expires and a watcher can forfeit the
// stale claim. The schema is internal/storage/migrations/011_task_leases.
//
// This file is the schema + acquire/renew/release/expiry primitives
// only. Heartbeat scheduling, reaping, and presence are follow-on quests
// in the daemon Phase 3 campaign; the TTL/interval constants below are
// hardcoded here and lift into config when the heartbeat quest lands.
//
// HARD INVARIANT (no-daemon byte-identical): task_leases rows are
// written ONLY by daemon-mediated accepts. The default quest_accept path
// runs with a nil lease seam and creates zero lease rows, so its DB
// effects stay byte-identical to today. A claim accepted without the
// daemon has no lease and therefore can never be falsely forfeited as
// expired later. See accept_cmd.go for the best-effort wire-in.

const (
	// DefaultLeaseTTL is how long a freshly acquired or heartbeated
	// lease stays valid without a renewal. A daemon heartbeats well
	// inside this window; a crashed agent stops heartbeating and the
	// lease lapses after one TTL. Hardcoded for the acquire slice;
	// the heartbeat quest lifts it into config.
	DefaultLeaseTTL = 90 * time.Second

	// DefaultHeartbeatInterval is the cadence the daemon's heartbeat
	// loop renews held leases at. Kept well below DefaultLeaseTTL so a
	// single missed beat does not expire a live lease. Consumed by the
	// follow-on heartbeat quest; defined here so the TTL/interval pair
	// lives in one place.
	DefaultHeartbeatInterval = 30 * time.Second
)

// Lease is one row of task_leases: a held claim plus its liveness
// timestamps. Returned by ExpiredLeases for the reaper to act on.
type Lease struct {
	ProjectID   string
	TaskID      string
	SessionID   string
	Holder      string
	AcquiredAt  string
	HeartbeatAt string
	ExpiresAt   string
}

// LeaseAcquirer is the seam quest_accept calls after a claim commits.
// command.Deps carries it as `any` (the field is Lease) to keep the
// command package free of a quest dependency, mirroring the Embed port
// (command.go Deps.Embed). The default seam is nil: no daemon, no lease,
// byte-identical to today. Only the daemon wires a non-nil implementation,
// pre-bound to its per-session identity, so the accept handler passes
// only what it knows (project, task, holder) and never has to source a
// session id it does not own.
//
// AcquireLease is best-effort from quest_accept's perspective: a failure
// is logged and never converts a committed claim into an API error, the
// same invariant as the accept trail writer (accept.go writeAcceptTrail).
type LeaseAcquirer interface {
	AcquireLease(ctx context.Context, projectID, taskID, holder string) error
}

// LeaseRenewer is the activity-renewal seam the mutating quest handlers
// (journal, campfire, update, fulfill) call after a successful write
// (ADR-005 Part 1, daemon Phase 3). When the calling session holds the
// lease for the touched (project, quest), the write refreshes that
// lease's heartbeat: cheap insurance against tick starvation under load,
// and it re-arms the lease of a session that re-accepted work after a
// daemon restart. A write that does not match a held lease is a no-op.
//
// command.Deps carries the implementation behind the same `any`-typed
// Lease field as LeaseAcquirer (the daemon wires a value that satisfies
// both), so the default nil seam is the no-daemon path: a mutating call
// writes exactly today's rows and touches no task_leases row. Renewal is
// best-effort: a failure is logged and never converts a committed write
// into an API error.
type LeaseRenewer interface {
	RenewLeaseActivity(ctx context.Context, projectID, taskID string) error
}

// DBLeaseAcquirer is the *sql.DB-backed LeaseAcquirer the daemon wires
// into command.Deps.Lease. SessionID is the daemon's per-session
// identity (from the multi-session serving loop); the daemon constructs
// one acquirer per session so the accept handler stays session-agnostic.
// Now is injectable for deterministic expiry math in tests (defaults to
// time.Now().UTC() when nil), mirroring the command.Deps.Now precedent.
//
// Terminal CLI accepts routed through the daemon are one-shot processes
// with no persistent session to heartbeat; v1 deliberately wires NO
// acquirer for them, preserving exactly today's terminal semantics with
// no false forfeits. This is a conscious v1 choice the daemon enforces
// by leaving Deps.Lease nil on that path.
type DBLeaseAcquirer struct {
	DB        *sql.DB
	SessionID string
	TTL       time.Duration
	Now       func() time.Time
}

// leaseFromDeps extracts the LeaseAcquirer the daemon stashed into
// command.Deps.Lease. command.Deps carries Lease as `any` to keep the
// command package free of a quest dependency; this helper centralizes
// the type assertion so the failure mode (nil on absence or mismatch) is
// uniform. A nil return is the documented no-daemon path: no lease row is
// ever written, so accept stays byte-identical to today.
func leaseFromDeps(d command.Deps) LeaseAcquirer {
	if d.Lease == nil {
		return nil
	}
	if a, ok := d.Lease.(LeaseAcquirer); ok {
		return a
	}
	return nil
}

// leaseRenewerFromDeps extracts the LeaseRenewer the daemon stashed into
// command.Deps.Lease, the activity-renewal sibling of leaseFromDeps. A
// nil return is the no-daemon path: the mutating handler skips renewal
// entirely and stays byte-identical to today.
func leaseRenewerFromDeps(d command.Deps) LeaseRenewer {
	if d.Lease == nil {
		return nil
	}
	if r, ok := d.Lease.(LeaseRenewer); ok {
		return r
	}
	return nil
}

// renewLeaseActivity is the shared best-effort activity renewal the
// mutating quest handlers call after a successful write. It is a no-op
// when no daemon wired a renewer (the byte-identical no-daemon path) and
// when the session does not hold the touched quest's lease; a renewal
// error is logged, never surfaced, because the underlying write already
// committed.
func renewLeaseActivity(ctx context.Context, d command.Deps, projectID, taskID string) {
	renewer := leaseRenewerFromDeps(d)
	if renewer == nil {
		return
	}
	if err := renewer.RenewLeaseActivity(ctx, projectID, taskID); err != nil {
		slog.Warn("quest: activity lease renewal failed; write is durable",
			"task_id", taskID,
			"error", err,
		)
	}
}

// AcquireLease writes the lease row for a just-committed claim, binding
// it to the acquirer's SessionID. It is the LeaseAcquirer implementation
// the accept seam invokes.
func (a *DBLeaseAcquirer) AcquireLease(ctx context.Context, projectID, taskID, holder string) error {
	ttl := a.TTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowFn := a.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return AcquireLease(ctx, a.DB, projectID, taskID, a.SessionID, holder, nowFn().UTC(), ttl)
}

// Close releases the long-lived quest.db handle the acquirer opened for
// its session. The daemon opens one acquirer per attached session and
// keeps the handle for the session's lifetime (so accepts and activity
// renewals do not re-open per call); detachSession calls Close when the
// connection drops so per-session handles do not accumulate over the
// daemon's life across connect/disconnect cycles. A nil acquirer or a nil
// DB is a safe no-op, and Close is idempotent: it nils DB after closing so
// a double Close (detach racing a shutdown) does not double-close the
// handle.
func (a *DBLeaseAcquirer) Close() error {
	if a == nil || a.DB == nil {
		return nil
	}
	db := a.DB
	a.DB = nil
	return db.Close()
}

// RenewLeaseActivity refreshes the touched quest's lease when this
// session holds it, the LeaseRenewer implementation the mutating quest
// handlers invoke. It scopes the renewal to (project, task, session) so a
// write to a quest some OTHER session leased never refreshes that other
// session's lease, and a write to an unleased quest is a no-op.
func (a *DBLeaseAcquirer) RenewLeaseActivity(ctx context.Context, projectID, taskID string) error {
	ttl := a.TTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowFn := a.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	_, err := RenewLeaseForSessionTask(ctx, a.DB, a.SessionID, projectID, taskID, nowFn().UTC(), ttl)
	return err
}

// AcquireLease records (or refreshes) the lease for taskID held by
// sessionID. acquired_at and heartbeat_at are set to now; expires_at to
// now+ttl. INSERT OR REPLACE so a daemon restart re-acquiring under a
// new session id refreshes the single (project_id, task_id) row rather
// than duplicating it.
//
// SQLITE_BUSY is retried up to three times, the same shape as the accept
// trail writer (accept.go writeAcceptTrail), because lease writes
// contend with the same WAL as concurrent accepts.
func AcquireLease(ctx context.Context, db *sql.DB, projectID, taskID, sessionID, holder string, now time.Time, ttl time.Duration) error {
	if db == nil {
		return fmt.Errorf("quest: acquire lease: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: acquire lease: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return fmt.Errorf("quest: acquire lease: empty task_id")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return fmt.Errorf("quest: acquire lease: empty session_id")
	}
	holder = agentOrDefault(holder)
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expStr := now.UTC().Add(ttl).Format(time.RFC3339Nano)

	return withBusyRetry(func() error {
		_, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO task_leases
			   (project_id, task_id, session_id, holder, acquired_at, heartbeat_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			projectID, taskID, sessionID, holder, nowStr, nowStr, expStr,
		)
		if err != nil {
			return fmt.Errorf("quest: acquire lease: insert: %w", err)
		}
		return nil
	})
}

// RenewLeasesForSession pushes heartbeat_at to now and expires_at to
// now+ttl for every lease sessionID holds. The daemon's heartbeat loop
// calls this on its interval. Returns the number of leases renewed.
func RenewLeasesForSession(ctx context.Context, db *sql.DB, sessionID string, now time.Time, ttl time.Duration) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("quest: renew leases: nil db")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return 0, fmt.Errorf("quest: renew leases: empty session_id")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expStr := now.UTC().Add(ttl).Format(time.RFC3339Nano)

	var affected int64
	err := withBusyRetry(func() error {
		res, err := db.ExecContext(ctx,
			`UPDATE task_leases
			 SET heartbeat_at = ?, expires_at = ?
			 WHERE session_id = ?`,
			nowStr, expStr, sessionID,
		)
		if err != nil {
			return fmt.Errorf("quest: renew leases: update: %w", err)
		}
		affected, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quest: renew leases: rows affected: %w", err)
		}
		return nil
	})
	return affected, err
}

// RenewLeaseForSessionTask pushes heartbeat_at to now and expires_at to
// now+ttl for the single lease (projectID, taskID) when sessionID holds
// it. It is the activity-renewal primitive: a mutating quest write
// refreshes its own session's lease without disturbing any lease another
// session holds. Returns the number of rows renewed (0 when the session
// does not hold that quest's lease, or no lease exists for it).
func RenewLeaseForSessionTask(ctx context.Context, db *sql.DB, sessionID, projectID, taskID string, now time.Time, ttl time.Duration) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("quest: renew lease: nil db")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return 0, fmt.Errorf("quest: renew lease: empty session_id")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return 0, fmt.Errorf("quest: renew lease: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return 0, fmt.Errorf("quest: renew lease: empty task_id")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expStr := now.UTC().Add(ttl).Format(time.RFC3339Nano)

	var affected int64
	err := withBusyRetry(func() error {
		res, err := db.ExecContext(ctx,
			`UPDATE task_leases
			 SET heartbeat_at = ?, expires_at = ?
			 WHERE session_id = ? AND project_id = ? AND task_id = ?`,
			nowStr, expStr, sessionID, projectID, taskID,
		)
		if err != nil {
			return fmt.Errorf("quest: renew lease: update: %w", err)
		}
		affected, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quest: renew lease: rows affected: %w", err)
		}
		return nil
	})
	return affected, err
}

// RenewAllLeases pushes heartbeat_at to now and expires_at to now+ttl for
// EVERY task_leases row, regardless of session. The daemon calls it once
// on boot, before any expiry scan could run, so a lease left behind by a
// crashed daemon gets one grace window in which its session can re-dial
// and re-establish a heartbeat (ADR-005 hard invariant: a daemon crash
// mid-session loses nothing). A session that fell back in-process holds no
// connection to refresh its old lease; boot grace plus the generous TTL
// bound the false-forfeit window. Returns the number of leases renewed.
func RenewAllLeases(ctx context.Context, db *sql.DB, now time.Time, ttl time.Duration) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("quest: renew all leases: nil db")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expStr := now.UTC().Add(ttl).Format(time.RFC3339Nano)

	var affected int64
	err := withBusyRetry(func() error {
		res, err := db.ExecContext(ctx,
			`UPDATE task_leases SET heartbeat_at = ?, expires_at = ?`,
			nowStr, expStr,
		)
		if err != nil {
			return fmt.Errorf("quest: renew all leases: update: %w", err)
		}
		affected, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quest: renew all leases: rows affected: %w", err)
		}
		return nil
	})
	return affected, err
}

// ReleaseLeasesForSession deletes every lease sessionID holds. The
// daemon calls this on a clean session shutdown so the session's claims
// drop their leases without waiting for expiry. Returns the number of
// leases released. Releasing a lease does NOT touch task_status: the
// claim's lifecycle (forfeit / fulfill) is independent of its lease.
func ReleaseLeasesForSession(ctx context.Context, db *sql.DB, sessionID string) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("quest: release leases: nil db")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return 0, fmt.Errorf("quest: release leases: empty session_id")
	}
	var affected int64
	err := withBusyRetry(func() error {
		res, err := db.ExecContext(ctx,
			`DELETE FROM task_leases WHERE session_id = ?`,
			sessionID,
		)
		if err != nil {
			return fmt.Errorf("quest: release leases: delete: %w", err)
		}
		affected, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quest: release leases: rows affected: %w", err)
		}
		return nil
	})
	return affected, err
}

// ExpiredLeases returns every lease whose expires_at is at or before now,
// ordered by (project_id, task_id) for deterministic output. The reaper
// (a follow-on quest) walks these to forfeit the underlying stale claims.
func ExpiredLeases(ctx context.Context, db *sql.DB, now time.Time) ([]Lease, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: expired leases: nil db")
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	rows, err := db.QueryContext(ctx,
		`SELECT project_id, task_id, session_id, holder, acquired_at, heartbeat_at, expires_at
		 FROM task_leases
		 WHERE expires_at <= ?
		 ORDER BY project_id, task_id`,
		nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: expired leases: query: %w", err)
	}
	defer rows.Close()

	var out []Lease
	for rows.Next() {
		var l Lease
		if err := rows.Scan(
			&l.ProjectID, &l.TaskID, &l.SessionID, &l.Holder,
			&l.AcquiredAt, &l.HeartbeatAt, &l.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("quest: expired leases: scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: expired leases: iterate: %w", err)
	}
	return out, nil
}

// ListLeaseQuestsForSession returns the task ids of every lease sessionID
// currently holds, ordered for deterministic output. The daemon's
// per-session heartbeat reconciles its in-memory presence view from this
// (the tick already opens a handle per session, so reporting the held set
// adds no extra round-trip to any hot path), and the daemon-status readout
// surfaces the ids per session. An empty session id or a session holding
// no leases returns an empty slice, not an error.
func ListLeaseQuestsForSession(ctx context.Context, db *sql.DB, sessionID string) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: list session leases: nil db")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT task_id
		 FROM task_leases
		 WHERE session_id = ?
		 ORDER BY task_id`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: list session leases: query: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return nil, fmt.Errorf("quest: list session leases: scan: %w", err)
		}
		out = append(out, taskID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: list session leases: iterate: %w", err)
	}
	return out, nil
}

// DeleteLease removes the lease for one (project_id, task_id). The reaper
// calls this after it has forfeited the stale claim. Deleting a missing
// lease is a no-op, not an error.
func DeleteLease(ctx context.Context, db *sql.DB, projectID, taskID string) error {
	if db == nil {
		return fmt.Errorf("quest: delete lease: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: delete lease: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return fmt.Errorf("quest: delete lease: empty task_id")
	}
	return withBusyRetry(func() error {
		_, err := db.ExecContext(ctx,
			`DELETE FROM task_leases WHERE project_id = ? AND task_id = ?`,
			projectID, taskID,
		)
		if err != nil {
			return fmt.Errorf("quest: delete lease: delete: %w", err)
		}
		return nil
	})
}

// withBusyRetry runs fn up to three times, retrying only on SQLITE_BUSY.
// Mirrors the accept trail writer's contention handling (accept.go
// writeAcceptTrail) so lease writes survive WAL contention with
// concurrent accepts without endangering any primary claim.
func withBusyRetry(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isBusyErr(err.Error()) {
			return err
		}
	}
	return lastErr
}
