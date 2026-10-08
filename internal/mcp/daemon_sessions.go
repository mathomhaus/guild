package mcp

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/quest"
)

// This file is the host's half of the daemon session registry + lease
// heartbeat (ADR-005 Phase 3). The registry in internal/daemon decides
// WHEN to act (a connection attaches, a tick fires, the daemon boots);
// these seams supply WHAT one action touches: open quest.db and call the
// lease primitives in internal/quest. Keeping it here means internal/daemon
// never imports internal/quest, exactly the leaf discipline the idle
// scheduler's PassFunc and the watch pipeline's ProcessFunc already follow.
//
// Everything below is gated on a running daemon: the registry only exists
// inside `guild daemon run`, leases are written only by daemon-mediated
// accepts, and the per-session acquirer is wired only when the registry is
// present. The no-daemon path (stdio server, in-process fallback) leaves
// command.Deps.Lease nil and writes zero lease rows, byte-identical to
// today.

// defaultDaemonLeaseTTL and defaultDaemonHeartbeatInterval are the
// built-in lease timings a daemon falls back to when config did not supply
// (or supplied a non-positive) value. They mirror the lease layer's
// hardcoded defaults so a daemon with no [daemon] lease config behaves
// identically to the lease primitives' own fallbacks.
const (
	defaultDaemonLeaseTTL          = 10 * time.Minute
	defaultDaemonHeartbeatInterval = 30 * time.Second
	// defaultDaemonReapInterval is how often the lease reaper sweeps for
	// expired leases when config did not supply a value. A minute keeps a
	// lapsed lease (a crashed agent's claim) returning to the board within
	// about one TTL plus one interval while the scan stays negligible load.
	defaultDaemonReapInterval = time.Minute
)

// daemonSessionID maps a shim pid to the daemon's per-session identity
// string. The same value keys the registry entry and every task_leases
// row the session takes, so the heartbeat tick renews exactly the rows the
// session's accepts wrote. Centralized so the registration and the lease
// acquirer can never drift on the encoding.
func daemonSessionID(pid int) string {
	return strconv.Itoa(pid)
}

// resolveSessionProject best-effort resolves the active project for a
// freshly attached session, for the registry's presence readout. An empty
// arg and env asks the store for the session's persisted active project; a
// session that has not bootstrapped one yet resolves to empty, which the
// registry records as an unbound session. A resolution error is swallowed
// (empty project): the lease rows, not this label, are the source of truth
// for what a session holds, so a missing project never blocks registration.
func (h *DaemonHost) resolveSessionProject(ctx context.Context, store SessionStore) string {
	proj, err := store.ResolveForMCP(ctx, "", "")
	if err != nil {
		return ""
	}
	return proj
}

// detachSession unregisters a session on connection close. It first runs a
// final lease renewal so the session's leases carry a full TTL at the
// moment the connection drops, narrowing the window a concurrently running
// reaper could observe a lease that lapsed only because the heartbeat tick
// had not landed yet. Renewal failure is non-fatal: the session is leaving
// regardless, and the lease's TTL still covers the gap.
//
// It then closes the session's lease acquirer, releasing the long-lived
// quest.db handle that acquirer held for the session's lifetime, so
// per-session handles do not accumulate over the daemon's life across
// connect/disconnect cycles. The acquirer is closed AFTER the final
// renewal because that renewal opens its own short-lived handle and does
// not depend on the acquirer's; closing is a safe no-op when no acquirer
// was wired (a db-open failure on attach degraded the session to no leases).
//
// sessionCtx is the (by-now cancelled) connection context. The final
// renewal needs a live deadline because that context is already done, so
// it runs under a short timeout derived with context.WithoutCancel:
// inherits the session's values, drops its cancellation, so the quick
// local write is not skipped just because the session ended.
func (h *DaemonHost) detachSession(sessionCtx context.Context, sessionID string, acquirer *quest.DBLeaseAcquirer) {
	if h.registry == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(sessionCtx), 2*time.Second)
	defer cancel()
	if err := h.renewLeasesForSession(ctx, sessionID); err != nil {
		h.logger.Warn("daemon: final session lease renewal failed on detach; relying on lease TTL",
			"session", sessionID, "err", err.Error())
	}
	if acquirer != nil {
		if err := acquirer.Close(); err != nil {
			h.logger.Warn("daemon: closing session lease db handle failed on detach",
				"session", sessionID, "err", err.Error())
		}
	}
	h.registry.Unregister(sessionID)
}

// newLeaseAcquirer builds the per-session quest-lease port threaded onto
// command.Deps.Lease. It opens a quest.db handle bound to the acquirer for
// the session's lifetime: a daemon session is long-lived, so the per-call
// open discipline the short-lived MCP tools use would mean re-opening on
// every accept; one handle per session is the right granularity here. The
// handle is closed by detachSession when the connection drops (via the
// acquirer's Close), so handles do not accumulate over the daemon's life.
// A db-open failure degrades to no lease acquirer (nil), so the session
// still serves and just behaves like the no-daemon path for leases.
func (h *DaemonHost) newLeaseAcquirer(ctx context.Context, sessionID string) *quest.DBLeaseAcquirer {
	db, err := openQuestDB(ctx)
	if err != nil {
		h.logger.Warn("daemon: lease acquirer unavailable; session runs without leases",
			"session", sessionID, "err", err.Error())
		return nil
	}
	return &quest.DBLeaseAcquirer{
		DB:        db,
		SessionID: sessionID,
		TTL:       h.leaseTTL,
	}
}

// reapSeam returns the daemon.ReapFunc the lease reaper's sweep tick calls.
// It opens a fresh quest.db handle and runs one quest.ReapExpiredLeases
// pass, threading the registry's IsLive predicate so a still-registered
// session's lapsed lease is a missed heartbeat (left for the tick to renew)
// rather than a crash. Per-call open matches the renewal and boot-grace
// seams; WAL makes it cheap and a sweep is infrequent (tens of seconds). A
// missing leases table (a quest.db that never took a lease) is a clean
// no-op because ExpiredLeases returns zero rows.
func (h *DaemonHost) reapSeam() daemon.ReapFunc {
	return func(ctx context.Context) (daemon.ReapOutcome, error) {
		db, err := openQuestDB(ctx)
		if err != nil {
			return daemon.ReapOutcome{}, fmt.Errorf("mcp: lease reap: open quest db: %w", err)
		}
		defer func() { _ = db.Close() }()

		isLive := func(sessionID string) bool { return false }
		if h.registry != nil {
			isLive = h.registry.IsLive
		}

		res, err := quest.ReapExpiredLeases(ctx, db, time.Now().UTC(), isLive)
		if err != nil {
			return daemon.ReapOutcome{}, fmt.Errorf("mcp: lease reap: %w", err)
		}
		return daemon.ReapOutcome{
			Scanned:        res.Scanned,
			Forfeited:      res.Forfeited,
			SkippedLive:    res.SkippedLive,
			OrphansCleared: res.OrphansCleared,
		}, nil
	}
}

// renewLeasesSeam returns the daemon.RenewFunc the heartbeat tick calls
// once per live session per tick. It opens a fresh quest.db handle and
// pushes every lease the session holds to a full TTL (heartbeat to now,
// expiry to now+TTL). Per-call open matches the watch and sleep seams; WAL
// makes it cheap, and a tick is infrequent (tens of seconds).
func (h *DaemonHost) renewLeasesSeam() daemon.RenewFunc {
	return func(ctx context.Context, sessionID string) error {
		return h.renewLeasesForSession(ctx, sessionID)
	}
}

// renewLeasesForSession is the shared renewal body the tick and the detach
// path both call: open quest.db, refresh every lease the session holds, and
// reconcile the registry's in-memory held-quest view from the same handle so
// the presence and daemon-status readouts stay current without a second
// round-trip. The reconciliation is best-effort: a list failure leaves the
// last-known held set in place and never fails the renewal, since the lease
// refresh (the durability-critical half) already succeeded.
func (h *DaemonHost) renewLeasesForSession(ctx context.Context, sessionID string) error {
	db, err := openQuestDB(ctx)
	if err != nil {
		return fmt.Errorf("mcp: renew session leases: open quest db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := quest.RenewLeasesForSession(ctx, db, sessionID, time.Now().UTC(), h.leaseTTL); err != nil {
		return fmt.Errorf("mcp: renew session leases: %w", err)
	}

	if h.registry != nil {
		if held, lerr := quest.ListLeaseQuestsForSession(ctx, db, sessionID); lerr == nil {
			h.registry.SetHeldQuests(sessionID, held)
		} else {
			h.logger.Warn("daemon: reconciling held-quest presence view failed; keeping last-known set",
				"session", sessionID, "err", lerr.Error())
		}
	}
	return nil
}

// bootGraceSeam returns the daemon.BootGraceFunc the registry calls once
// on daemon start: renew every pre-existing task_leases row to a full TTL
// before any reaper could run, so a lease left behind by a crashed daemon
// gets one grace window for its session to re-dial. Opens a fresh quest.db
// handle; a missing leases table (a quest.db that never took a lease) is a
// clean no-op because RenewAllLeases updates zero rows.
func (h *DaemonHost) bootGraceSeam() daemon.BootGraceFunc {
	return func(ctx context.Context) error {
		db, err := openQuestDB(ctx)
		if err != nil {
			return fmt.Errorf("mcp: lease boot grace: open quest db: %w", err)
		}
		defer func() { _ = db.Close() }()

		if _, err := quest.RenewAllLeases(ctx, db, time.Now().UTC(), h.leaseTTL); err != nil {
			return fmt.Errorf("mcp: lease boot grace: %w", err)
		}
		return nil
	}
}
