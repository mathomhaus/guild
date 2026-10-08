package daemon

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// This file is the daemon's lease reaper (ADR-005 Part 1, "Why a daemon"
// item 3; phasing table Phase 3). It is the payoff the lease layer exists
// for: a crashed agent's shim dies, its connection drops, its session
// unregisters, the heartbeat stops, the lease lapses one TTL later, and on
// the next reaper tick the zombie claim is auto-forfeited back to the board
// instead of rotting in_progress until a human notices.
//
// Like the idle scheduler and the heartbeat tick, the reaper only decides
// WHEN to sweep. WHAT a sweep does lives in internal/quest
// (ReapExpiredLeases: scan task_leases, guard on session liveness and claim
// identity, forfeit, clean up). The host wires a ReapFunc that opens
// quest.db and calls it, so internal/daemon stays free of internal/quest,
// exactly the leaf split the renewal RenewFunc and the sleep PassFunc use.
//
// Hard invariants from the ADR carried here:
//
//   - Zero impact on the no-daemon path: the reaper runs ONLY inside a
//     running daemon, and it scans task_leases, never task_status. A claim
//     accepted without the daemon writes no lease row, so it is invisible to
//     the reaper by construction and can never be falsely forfeited. No
//     leases, no false forfeits.
//   - The reaper must not block serving: it runs in its own goroutine on its
//     own ticker, and a sweep is a bounded scan-and-forfeit over expired rows
//     only.
//   - A sweep error (SQLITE_BUSY, a transient db error) is logged and the
//     next tick retries; it is never fatal to the daemon.
//   - Joins shutdown cleanly: Run returns on ctx cancellation so the daemon's
//     WaitGroup completes.

// defaultReapInterval is the reaper's sweep cadence when none is
// configured. A minute is frequent enough that a lapsed lease (a crashed
// agent's claim) returns to the board within about one TTL plus one
// interval, and infrequent enough that the scan is negligible load.
const defaultReapInterval = time.Minute

// minReapInterval floors the sweep cadence so a tiny configured interval
// (or a fast-clock test) cannot spin the reaper loop.
const minReapInterval = time.Second

// ReapOutcome is the minimal result a ReapFunc reports back so the reaper
// can surface a sweep's effect on the daemon's logs (and, later, the
// daemon-status/presence readout) without importing internal/quest's
// ReapResult shape.
type ReapOutcome struct {
	// Scanned is how many expired lease rows the sweep examined.
	Scanned int
	// Forfeited is how many zombie claims the sweep auto-released.
	Forfeited int
	// SkippedLive is how many expired leases the sweep left alone because
	// their session was still registered (a missed heartbeat, not a crash).
	SkippedLive int
	// OrphansCleared is how many stale lease rows the sweep deleted without
	// forfeiting (the claim was already re-assigned, fulfilled, or done).
	OrphansCleared int
}

// ReapFunc runs one lease sweep and reports its outcome. The host supplies
// it: it opens a fresh quest.db handle and calls quest.ReapExpiredLeases
// with the registry's liveness predicate, so internal/daemon never imports
// internal/quest. It must honor ctx cancellation (daemon shutdown). A
// ReapFunc returning an error is logged; the reaper keeps ticking and the
// next sweep retries, so one stuck sweep never wedges the loop.
type ReapFunc func(ctx context.Context) (ReapOutcome, error)

// ReaperConfig parameterizes a Reaper. The zero value is a usable reaper
// whose tick never sweeps (no Reap func), so a host wiring gap degrades to
// "no reaping" instead of crashing the daemon.
type ReaperConfig struct {
	// Interval is the sweep cadence. Non-positive uses defaultReapInterval;
	// it is floored at minReapInterval.
	Interval time.Duration
	// Reap runs one sweep. Nil disables sweeping (the reaper loop still runs
	// so shutdown stays clean), so a wiring gap is inert.
	Reap ReapFunc
	// Logger receives sweep/degradation diagnostics. Nil falls back to
	// slog.Default(); never logs to stdout.
	Logger *slog.Logger
	// clock is injected by tests. nil uses the real clock (the same clock
	// seam the scheduler and registry use).
	clock clock
}

// Reaper periodically forfeits zombie claims behind expired leases. It runs
// one goroutine (its ticker loop); construct with NewReaper, drive with Run.
type Reaper struct {
	cfg      ReaperConfig
	logger   *slog.Logger
	clk      clock
	interval time.Duration

	// totalForfeited is the lifetime count of zombie claims this reaper has
	// forfeited since the daemon started. Atomic because the sweep
	// goroutine writes it while a status-request goroutine reads it through
	// TotalForfeited. The daemon-status readout surfaces it so an operator
	// can see at a glance how many crashed agents' claims the daemon has
	// returned to the board.
	totalForfeited atomic.Int64

	// onSwept, when non-nil, is called at the very end of sweep() (after the
	// forfeit tally and the decision record). It is a test-only barrier seam:
	// a test can synchronize on a sweep being fully complete without driving a
	// second "barrier" tick that would race in an extra sweep. Nil in
	// production, so the live sweep path is unchanged.
	onSwept func()
}

// NewReaper validates cfg and returns a runnable Reaper.
func NewReaper(cfg ReaperConfig) *Reaper {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clk := cfg.clock
	if clk == nil {
		clk = realClock{}
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultReapInterval
	}
	if interval < minReapInterval {
		interval = minReapInterval
	}
	return &Reaper{
		cfg:      cfg,
		logger:   logger,
		clk:      clk,
		interval: interval,
	}
}

// Run drives the sweep tick until ctx is cancelled (daemon shutdown). Each
// tick runs one sweep. A nil ReapFunc (host wiring gap) makes the loop tick
// without touching the db, so the daemon still serves; it just never reaps.
// Run blocks; the daemon starts it in its own goroutine alongside the
// scheduler, the watch pipeline, and the heartbeat tick.
func (r *Reaper) Run(ctx context.Context) {
	r.logger.Info("daemon: lease reaper started", "interval", r.interval.String())

	tk := r.clk.NewTicker(r.interval)
	defer tk.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			r.sweep(ctx)
		}
	}
}

// sweep runs one lease sweep. A nil ReapFunc is a no-op. A sweep error is
// logged and the next tick retries (cancellation during shutdown is not a
// failure). A sweep that forfeited or cleared anything logs a summary line;
// an empty sweep stays quiet so a healthy idle daemon does not spam its log.
func (r *Reaper) sweep(ctx context.Context) {
	// Signal sweep completion to a test barrier, if one is installed, on every
	// return path. Nil in production. This lets a test wait for the sweep to
	// finish (tally added, decision recorded) without a stray second tick.
	if r.onSwept != nil {
		defer r.onSwept()
	}
	if r.cfg.Reap == nil {
		return
	}
	out, err := r.cfg.Reap(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		r.logger.Warn("daemon: lease reap sweep failed; will retry next tick", "err", err.Error())
		// Record the failed sweep so an operator can see reap errors in the
		// event stream (ADR-006 Phase 5). No-op unless a recorder is wired.
		recordDecision(Decision{
			Kind:   DecisionLeaseReap,
			Allow:  false,
			Reason: "sweep_error",
			At:     r.clk.Now(),
			Inputs: map[string]bool{"swept": true, "errored": true, "forfeited_any": false},
		})
		return
	}
	if out.Forfeited > 0 {
		r.totalForfeited.Add(int64(out.Forfeited))
	}
	// Record the sweep decision (ADR-006 Phase 5). This does NOT change what
	// the sweep did: the ReapFunc already ran and out is its result; we only
	// snapshot it. The gate's Allow is "did this sweep forfeit anything",
	// the actionable signal. Recording is a no-op unless the observability
	// module installed a recorder, so the disabled path is byte-identical.
	recordDecision(Decision{
		Kind:   DecisionLeaseReap,
		Allow:  out.Forfeited > 0,
		Reason: reapReason(out),
		At:     r.clk.Now(),
		Inputs: map[string]bool{
			"swept":           true,
			"errored":         false,
			"forfeited_any":   out.Forfeited > 0,
			"skipped_live":    out.SkippedLive > 0,
			"orphans_cleared": out.OrphansCleared > 0,
		},
		Metrics: map[string]int{
			"scanned":         out.Scanned,
			"forfeited":       out.Forfeited,
			"skipped_live":    out.SkippedLive,
			"orphans_cleared": out.OrphansCleared,
		},
	})
	if out.Forfeited > 0 || out.OrphansCleared > 0 {
		r.logger.Info("daemon: lease reaper swept",
			"scanned", out.Scanned,
			"forfeited", out.Forfeited,
			"skipped_live", out.SkippedLive,
			"orphans_cleared", out.OrphansCleared,
			"total_forfeited", r.totalForfeited.Load(),
		)
	}
}

// TotalForfeited returns the lifetime count of zombie claims this reaper has
// forfeited since the daemon started. Safe for concurrent use: a
// status-request goroutine reads it while the sweep goroutine writes it.
func (r *Reaper) TotalForfeited() int64 {
	return r.totalForfeited.Load()
}

// reapReason summarizes a successful sweep's outcome for the decision
// record's human-readable reason. It only describes what the sweep already
// did; it never affects the sweep.
func reapReason(out ReapOutcome) string {
	switch {
	case out.Forfeited > 0:
		return "forfeited_zombie_claims"
	case out.OrphansCleared > 0:
		return "cleared_orphans_only"
	case out.SkippedLive > 0:
		return "all_expired_live"
	default:
		return "nothing_to_reap"
	}
}
