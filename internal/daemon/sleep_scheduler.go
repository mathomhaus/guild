package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// This file is the daemon's idle dream-pass scheduler (ADR-005 Phase 2).
// The resident daemon (Phase 1) is the first time guild has a process
// that outlives a single tool call; the scheduler is the thing that
// process exists for. It watches a last-activity timestamp that every
// MCP request and every CLI exec RPC bumps, and after IdleMinutes of
// silence it spends ONE bounded pass on autonomous maintenance.
//
// The scheduler only decides WHEN. WHAT a pass does lives in the sleep
// runner and its registered steps (internal/sleep); the daemon never
// imports the mutation logic. The host wires a PassFunc that builds the
// per-pass sleep.PassContext (db handles, embed deps) and calls
// sleep.Run, so internal/daemon stays free of internal/mcp.
//
// Hard invariants from the ADR carried here:
//
//   - Zero impact on the no-daemon path: nothing in this file runs
//     unless `guild daemon run` started a Scheduler.
//   - Dreaming never makes a waking session feel slower: new activity
//     preempts an in-flight pass by cancelling its context, and a pass
//     runs under a wall budget the runner enforces.
//   - At most one pass at a time, and a new pass never starts within
//     IdleMinutes of the previous pass ending.

// minTickInterval floors the scheduler's poll cadence. The ticker fires
// at a fraction of the idle window (see tickInterval) so a pass becomes
// due within a fraction of IdleMinutes of the machine going quiet;
// flooring keeps a tiny IdleMinutes (or a fast-clock test) from spinning
// the loop.
const minTickInterval = time.Second

// tickInterval derives the poll cadence from the idle window: a quarter
// of it, floored at minTickInterval. A quarter means the worst-case
// extra delay before a due pass fires is IdleMinutes/4, cheap relative
// to the idle window itself.
func tickInterval(idle time.Duration) time.Duration {
	d := idle / 4
	if d < minTickInterval {
		return minTickInterval
	}
	return d
}

// clock is the scheduler's view of time, injectable so tests drive the
// idle math deterministically without sleeping. nil defaults to the
// real clock.
type clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTicker returns a ticker firing every d.
	NewTicker(d time.Duration) ticker
}

// ticker abstracts time.Ticker so a fake clock can deliver ticks on
// demand.
type ticker interface {
	C() <-chan time.Time
	Stop()
}

// realClock is the production clock backed by the time package.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTicker(d time.Duration) ticker {
	return &realTicker{t: time.NewTicker(d)}
}

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// PassOutcome is the minimal result a PassFunc reports back so the
// scheduler can surface "last sleep pass" on daemon status without
// importing internal/sleep's PassResult shape.
type PassOutcome struct {
	// Partial is true when the wall budget (or a preempting request)
	// cut the pass short before every step finished.
	Partial bool
	// Steps counts the steps the pass drove (zero while the step
	// registry is empty).
	Steps int
}

// PassFunc runs one bounded dream pass and reports its outcome. The
// host supplies it: it builds the sleep.PassContext (lore/quest db
// handles, embed deps) for trigger "daemon-idle", calls sleep.Run with
// the registered steps under budget, and maps the result to a
// PassOutcome. It must honor ctx cancellation promptly so a waking
// session preempts a running pass.
//
// A PassFunc returning an error is logged; the scheduler still records
// the pass as ended (the runner journals the pass row regardless) so a
// failed pass does not wedge the gap timer.
type PassFunc func(ctx context.Context, budget time.Duration) (PassOutcome, error)

// LastPass is a snapshot of the most recent dream pass for daemon
// status. Started is zero when no pass has run since this daemon
// started.
type LastPass struct {
	// Started is when the pass began.
	Started time.Time
	// Ended is when the pass returned. Zero while a pass is in flight.
	Ended time.Time
	// Partial mirrors PassOutcome.Partial for the last completed pass.
	Partial bool
	// Steps mirrors PassOutcome.Steps for the last completed pass.
	Steps int
	// Err is the last pass's PassFunc error, empty on success.
	Err string
}

// SchedulerConfig parameterizes a Scheduler. The daemon resolves these
// from config.SleepConfig; the zero value never fires (Idle <= 0).
type SchedulerConfig struct {
	// Enabled gates the whole scheduler. False means the loop runs but
	// never fires a pass (so Touch stays cheap and status still answers).
	Enabled bool
	// Idle is how long the daemon must see no activity before a pass is
	// due, and the minimum gap between one pass ending and the next
	// starting. Non-positive disables firing.
	Idle time.Duration
	// Budget is the wall budget handed to each pass.
	Budget time.Duration
	// Pass runs one bounded pass. Required when Enabled; nil disables
	// firing.
	Pass PassFunc
	// Logger receives start/end diagnostics. Nil falls back to
	// slog.Default().
	Logger *slog.Logger
	// clock is injected by tests. nil uses the real clock.
	clock clock
}

// Scheduler watches daemon activity and fires bounded dream passes. It
// is safe for concurrent use: Touch is called from every connection
// goroutine while Run owns the ticker loop.
type Scheduler struct {
	cfg    SchedulerConfig
	log    *slog.Logger
	clk    clock
	budget time.Duration

	mu sync.Mutex
	// lastActivity is the most recent Touch (or the scheduler start, so
	// a freshly started idle daemon waits a full Idle window before its
	// first pass rather than firing immediately).
	lastActivity time.Time
	// lastPassEnded is when the previous pass returned; the gap rule
	// measures from here. Zero means no pass has run yet.
	lastPassEnded time.Time
	// running guards the single-flight invariant.
	running bool
	// cancelPass cancels an in-flight pass when new activity arrives.
	// nil when no pass is running.
	cancelPass context.CancelFunc
	// last is the most recent pass snapshot for status.
	last LastPass
}

// NewScheduler validates cfg and returns a runnable Scheduler. lastActivity
// is seeded to "now" so a daemon that starts idle still waits a full
// Idle window before its first pass.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	clk := cfg.clock
	if clk == nil {
		clk = realClock{}
	}
	s := &Scheduler{
		cfg:    cfg,
		log:    log,
		clk:    clk,
		budget: cfg.Budget,
	}
	s.lastActivity = clk.Now()
	return s
}

// Touch records activity now and preempts any in-flight pass. Every MCP
// session start and every CLI exec RPC calls it. It is cheap and
// lock-bounded so it never delays serving.
func (s *Scheduler) Touch() {
	s.mu.Lock()
	s.lastActivity = s.clk.Now()
	cancel := s.cancelPass
	s.mu.Unlock()
	// Cancel outside the lock: the pass goroutine grabs s.mu to record
	// its end, so cancelling under the lock could serialize needlessly.
	if cancel != nil {
		cancel()
	}
}

// Last returns a snapshot of the most recent dream pass for daemon
// status. The zero value means no pass has run since this daemon
// started.
func (s *Scheduler) Last() LastPass {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// canFire reports whether a pass is due as of now: the scheduler is
// armed, no pass is running, the daemon has been idle for at least Idle,
// and the previous pass (if any) ended at least Idle ago. Caller holds
// s.mu.
//
// It is a thin wrapper over autopassDecision so the firing logic has a
// single source of truth: the bool it returns is exactly the value object's
// Allow field, and recording (when wired) cannot drift from it.
func (s *Scheduler) canFire(now time.Time) bool {
	return s.autopassDecision(now).Allow
}

// autopassDecision computes the frozen value object for the "fire a dream
// pass now?" gate (ADR-006 Phase 5). It captures every boolean the gate
// considers plus the decisive reason, WITHOUT changing the gate: Allow is
// the same conjunction canFire always computed and the precedence of the
// short-circuit checks is preserved. Caller holds s.mu (it reads
// lastActivity / lastPassEnded / running).
func (s *Scheduler) autopassDecision(now time.Time) Decision {
	armed := s.cfg.Enabled && s.cfg.Pass != nil && s.cfg.Idle > 0
	notRunning := !s.running
	idleElapsed := armed && now.Sub(s.lastActivity) >= s.cfg.Idle
	gapElapsed := s.lastPassEnded.IsZero() || (armed && now.Sub(s.lastPassEnded) >= s.cfg.Idle)

	allow := false
	reason := "fire"
	switch {
	case !armed:
		reason = "disabled"
	case !notRunning:
		reason = "pass_in_flight"
	case !idleElapsed:
		reason = "not_idle"
	case !gapElapsed:
		reason = "gap_not_elapsed"
	default:
		allow = true
	}

	return Decision{
		Kind:   DecisionAutopass,
		Allow:  allow,
		Reason: reason,
		At:     now,
		Inputs: map[string]bool{
			"armed":        armed,
			"not_running":  notRunning,
			"idle_elapsed": idleElapsed,
			"gap_elapsed":  gapElapsed,
		},
	}
}

// Run drives the ticker loop until ctx is cancelled (daemon shutdown).
// Each tick checks whether a pass is due and, if so, fires one. A
// disabled scheduler (or one with a non-positive Idle) still loops so
// shutdown stays clean, but never fires. Run blocks; the daemon starts
// it in its own goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	// A non-firing scheduler has no useful cadence; idle into ctx so the
	// caller's goroutine bookkeeping (WaitGroup) still completes on
	// shutdown without spinning a pointless ticker.
	if !s.cfg.Enabled || s.cfg.Pass == nil || s.cfg.Idle <= 0 {
		s.log.Info("daemon: sleep scheduler disabled",
			"enabled", s.cfg.Enabled,
			"idle", s.cfg.Idle.String(),
		)
		<-ctx.Done()
		return
	}

	interval := tickInterval(s.cfg.Idle)
	tk := s.clk.NewTicker(interval)
	defer tk.Stop()

	s.log.Info("daemon: sleep scheduler started",
		"idle", s.cfg.Idle.String(),
		"budget", s.budget.String(),
		"tick", interval.String(),
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			s.maybeFire(ctx)
		}
	}
}

// maybeFire fires one pass if due. It runs the pass synchronously on the
// loop goroutine: the single-flight flag means the loop is the only
// thing that can start a pass, and running it inline keeps the gap-timer
// bookkeeping race-free. A pass cannot starve the loop because it is
// wall-budgeted and preempted by Touch.
func (s *Scheduler) maybeFire(ctx context.Context) {
	s.mu.Lock()
	now := s.clk.Now()
	// Compute the frozen autopass decision once and branch on its Allow
	// field, so the recorded record and the taken branch can never disagree
	// (ADR-006 Phase 5). Recording is a no-op unless the observability module
	// installed a recorder, so this is byte-identical when disabled.
	dec := s.autopassDecision(now)
	if !dec.Allow {
		s.mu.Unlock()
		recordDecision(dec)
		return
	}
	// Arm: mark running and wire a cancellable pass context BEFORE
	// releasing the lock, so a Touch racing the fire sees cancelPass and
	// preempts the pass it could not block.
	passCtx, cancel := context.WithCancel(ctx)
	s.running = true
	s.cancelPass = cancel
	s.last = LastPass{Started: now}
	s.mu.Unlock()

	// Record the fire decision (Allow=true) once armed but before the pass
	// runs, so the recorder sees the same decision stream as the skip path.
	recordDecision(dec)

	s.log.Info("daemon: sleep pass firing", "trigger", "daemon-idle", "budget", s.budget.String())

	outcome, err := s.cfg.Pass(passCtx, s.budget)
	cancel()

	s.mu.Lock()
	ended := s.clk.Now()
	s.running = false
	s.cancelPass = nil
	s.lastPassEnded = ended
	s.last.Ended = ended
	s.last.Partial = outcome.Partial
	s.last.Steps = outcome.Steps
	if err != nil {
		s.last.Err = err.Error()
	}
	s.mu.Unlock()

	if err != nil {
		s.log.Warn("daemon: sleep pass failed", "err", err.Error())
		return
	}
	s.log.Info("daemon: sleep pass ended",
		"partial", outcome.Partial,
		"steps", outcome.Steps,
		"duration", ended.Sub(now).String(),
	)
}
