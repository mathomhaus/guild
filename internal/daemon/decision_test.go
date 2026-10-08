package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/daemon/watch"
)

// captureRecorder is a test DecisionRecorder that stores every recorded
// decision for assertions. Safe for concurrent use (the loops record off
// their own goroutines).
type captureRecorder struct {
	mu   sync.Mutex
	recs []Decision
}

func (c *captureRecorder) Record(d Decision) {
	c.mu.Lock()
	c.recs = append(c.recs, d)
	c.mu.Unlock()
}

func (c *captureRecorder) all() []Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Decision, len(c.recs))
	copy(out, c.recs)
	return out
}

func (c *captureRecorder) byKind(k DecisionKind) []Decision {
	var out []Decision
	for _, d := range c.all() {
		if d.Kind == k {
			out = append(out, d)
		}
	}
	return out
}

// sinkMu serializes access to the process-global decision sink across tests.
// The sink (decisionSink in decision.go) is a single package-global, so two
// tests that each SetDecisionRecorder concurrently would clobber each other's
// recorder and lose records. Every sink-using test holds this mutex for its
// whole duration via installRecorder, so sink installs never interleave even
// under the full -race ./... run.
var sinkMu sync.Mutex

// installRecorder sets the package sink to r and restores the no-op default
// (nil) at test end. It first takes sinkMu so no other sink-using test runs
// concurrently, then releases it in the same Cleanup, so the global sink is
// never clobberable across tests and every test starts from the default.
func installRecorder(t *testing.T, r DecisionRecorder) {
	t.Helper()
	sinkMu.Lock()
	SetDecisionRecorder(r)
	t.Cleanup(func() {
		SetDecisionRecorder(nil)
		sinkMu.Unlock()
	})
}

// TestRecordDecision_NoRecorder_IsNoop verifies the default state (no
// recorder installed) is a safe no-op: this is the disabled-observability
// path the parity bar requires.
func TestRecordDecision_NoRecorder_IsNoop(t *testing.T) {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	SetDecisionRecorder(nil)
	// Must not panic and must record nothing observable.
	recordDecision(Decision{Kind: DecisionAutopass, Allow: true, Reason: "fire"})
}

// TestAutopassDecision_CapturesBooleans checks the autopass value object
// captures every input boolean and the right reason for each short-circuit,
// WITHOUT the recorded Allow diverging from canFire's bool.
func TestAutopassDecision_CapturesBooleans(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		cfg        SchedulerConfig
		setup      func(s *Scheduler)
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "disabled",
			cfg:        SchedulerConfig{Enabled: false},
			wantAllow:  false,
			wantReason: "disabled",
		},
		{
			name: "pass_in_flight",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.running = true
				s.lastActivity = now.Add(-2 * time.Minute)
			},
			wantAllow:  false,
			wantReason: "pass_in_flight",
		},
		{
			name: "not_idle",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.lastActivity = now // just touched
			},
			wantAllow:  false,
			wantReason: "not_idle",
		},
		{
			name: "gap_not_elapsed",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.lastActivity = now.Add(-2 * time.Minute)
				s.lastPassEnded = now.Add(-30 * time.Second) // inside the gap
			},
			wantAllow:  false,
			wantReason: "gap_not_elapsed",
		},
		{
			name: "fire",
			cfg:  SchedulerConfig{Enabled: true, Idle: time.Minute, Pass: noopPass},
			setup: func(s *Scheduler) {
				s.lastActivity = now.Add(-2 * time.Minute)
			},
			wantAllow:  true,
			wantReason: "fire",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Logger = quietLogger()
			s := NewScheduler(tc.cfg)
			s.lastActivity = now // default seed; setup overrides
			if tc.setup != nil {
				tc.setup(s)
			}
			d := s.autopassDecision(now)
			if d.Allow != tc.wantAllow {
				t.Errorf("Allow = %v, want %v", d.Allow, tc.wantAllow)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.wantReason)
			}
			// The value object's Allow must match canFire's bool: parity.
			if got := s.canFire(now); got != d.Allow {
				t.Errorf("canFire=%v but decision.Allow=%v: must agree", got, d.Allow)
			}
			// Every boolean input must be present.
			for _, k := range []string{"armed", "not_running", "idle_elapsed", "gap_elapsed"} {
				if _, ok := d.Inputs[k]; !ok {
					t.Errorf("missing input boolean %q", k)
				}
			}
		})
	}
}

func noopPass(context.Context, time.Duration) (PassOutcome, error) {
	return PassOutcome{}, nil
}

// TestScheduler_RecordsAutopass_OnFireAndSkip verifies the scheduler records
// both a skipped tick (not due) and a fired tick, and that recording does not
// change firing behavior.
func TestScheduler_RecordsAutopass_OnFireAndSkip(t *testing.T) {
	rec := &captureRecorder{}
	installRecorder(t, rec)

	fired := make(chan struct{}, 1)
	clk := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    time.Minute,
		Budget:  time.Second,
		clock:   clk,
		Logger:  quietLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			fired <- struct{}{}
			return PassOutcome{Steps: 1}, nil
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	// Not idle yet: this tick records a skip and does not fire. The
	// unbuffered fake clock means tick() returns once the loop receives it;
	// the loop runs maybeFire (the skip record) before it can receive the
	// next tick, so by the time the firing tick below is received, this
	// skip record is committed. No second "barrier" tick (which would drive
	// an extra, racy iteration) is needed.
	clk.tick()

	// Advance past idle so the next tick fires. Because the loop processes
	// ticks serially, it cannot receive this tick until the prior skip's
	// maybeFire returned; <-fired then proves the fire record (written
	// before Pass runs) is committed AND the skip before it is too.
	clk.advance(2 * time.Minute)
	clk.tick()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("pass did not fire after idle elapsed")
	}

	// Exactly one skip and one fire must have been recorded, in that order:
	// the skip tick records Allow=false, the fire tick records Allow=true.
	autopasses := rec.byKind(DecisionAutopass)
	if len(autopasses) != 2 {
		t.Fatalf("recorded autopass decisions = %d, want exactly 2 (one skip, one fire)", len(autopasses))
	}
	skip := autopasses[0]
	if skip.Allow {
		t.Fatalf("first decision: expected skip (not idle), got Allow=true reason=%q", skip.Reason)
	}
	fire := autopasses[1]
	if !fire.Allow || fire.Reason != "fire" {
		t.Errorf("second decision: want Allow=true reason=fire, got Allow=%v reason=%q", fire.Allow, fire.Reason)
	}
}

// TestReaper_RecordsSweepDecision verifies the reaper records the sweep
// outcome with the right booleans and metrics, and that recording does not
// change the forfeited tally.
func TestReaper_RecordsSweepDecision(t *testing.T) {
	rec := &captureRecorder{}
	installRecorder(t, rec)

	r, clk := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap: func(context.Context) (ReapOutcome, error) {
			return ReapOutcome{Scanned: 3, Forfeited: 2, SkippedLive: 1}, nil
		},
	})
	// onSwept is a post-sweep barrier: sweep() signals it AFTER the forfeit
	// tally and the decision record, so the test can wait for exactly one
	// sweep to fully complete without a second "barrier" tick that would race
	// in an extra sweep. Buffered so the reaper never blocks on it.
	swept := make(chan struct{}, 1)
	r.onSwept = func() { swept <- struct{}{} }

	stop := runReaper(t, r)
	defer stop()

	// Drive EXACTLY one sweep, then wait for it to finish.
	clk.tick()
	select {
	case <-swept:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep did not complete within 2s")
	}

	recs := rec.byKind(DecisionLeaseReap)
	if len(recs) != 1 {
		t.Fatalf("recorded lease_reap decisions = %d, want exactly 1", len(recs))
	}
	d := recs[0]
	if !d.Allow {
		t.Errorf("Allow = false, want true (forfeited 2)")
	}
	if d.Reason != "forfeited_zombie_claims" {
		t.Errorf("Reason = %q, want forfeited_zombie_claims", d.Reason)
	}
	if d.Metrics["forfeited"] != 2 {
		t.Errorf("forfeited metric = %d, want 2", d.Metrics["forfeited"])
	}
	if d.Inputs["errored"] {
		t.Error("errored input should be false on a clean sweep")
	}
	// Parity, asserted exactly and INDEPENDENTLY of the record count: one
	// sweep forfeits exactly 2, and recording does not inflate the tally (the
	// totalForfeited add lives in sweep(), untouched by recordDecision).
	if got := r.TotalForfeited(); got != 2 {
		t.Errorf("TotalForfeited = %d, want 2 (one sweep forfeits 2; recording must not change it)", got)
	}
}

// TestPipeline_RecordsStaleRenewDecision verifies the watch pipeline records
// the staleness-renewal decision for a processed event with the right
// booleans and metrics, and that recording does not change the counters the
// ProcessFunc produced. Synchronization is on the captured record itself
// (committed inside handle() after ProcessFunc returns), not a fixed sleep.
func TestPipeline_RecordsStaleRenewDecision(t *testing.T) {
	rec := &captureRecorder{}
	installRecorder(t, rec)

	root := t.TempDir()
	tracked := filepath.Join(root, "doc.md")
	if err := os.WriteFile(tracked, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	p := NewPipeline(PipelineConfig{
		Enabled:  true,
		Debounce: pipelineDebounce,
		Roots:    staticRoots(watch.Root{Project: "proj", Path: root}),
		Process: func(context.Context, watch.Event) (EventResult, error) {
			// One signal and one renewal per event so Allow is true and the
			// metrics are distinguishable.
			return EventResult{Signals: 1, QuestsPosted: 1}, nil
		},
		Logger: discardLogger(),
	})
	stop := runPipeline(t, p)
	defer stop()

	waitFor(t, "watcher to arm", func() bool { return p.Status().Watching })
	time.Sleep(pipelineArmDelay)

	if err := os.WriteFile(tracked, []byte("v2"), 0o600); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	waitFor(t, "a stale_renew decision to be recorded", func() bool {
		return len(rec.byKind(DecisionStaleRenew)) >= 1
	})

	recs := rec.byKind(DecisionStaleRenew)
	d := recs[0]
	if !d.Allow {
		t.Errorf("Allow = false, want true (one signal, one renewal)")
	}
	if d.Reason != "flagged_stale_and_posted_renewals" {
		t.Errorf("Reason = %q, want flagged_stale_and_posted_renewals", d.Reason)
	}
	if d.Inputs["errored"] {
		t.Error("errored input should be false on a processed event")
	}
	if !d.Inputs["signals_written"] || !d.Inputs["quests_posted"] {
		t.Errorf("inputs: want signals_written and quests_posted true, got %+v", d.Inputs)
	}
	if d.Metrics["signals"] != 1 || d.Metrics["quests"] != 1 {
		t.Errorf("metrics: want signals=1 quests=1, got signals=%d quests=%d", d.Metrics["signals"], d.Metrics["quests"])
	}
	// Parity: recording does not change the pipeline's own counters. The
	// ProcessFunc returned one signal and one quest; those land in Status
	// regardless of the record.
	st := p.Status()
	if st.SignalsRecorded < 1 || st.QuestsPosted < 1 {
		t.Errorf("pipeline counters: want signals>=1 quests>=1, got signals=%d quests=%d", st.SignalsRecorded, st.QuestsPosted)
	}
}

// TestSetDecisionRecorder_Removable verifies installing then removing the
// recorder restores the no-op path: a decision after removal is not recorded.
func TestSetDecisionRecorder_Removable(t *testing.T) {
	sinkMu.Lock()
	defer func() {
		SetDecisionRecorder(nil)
		sinkMu.Unlock()
	}()
	rec := &captureRecorder{}
	SetDecisionRecorder(rec)
	recordDecision(Decision{Kind: DecisionAutopass, Allow: true})
	if len(rec.all()) != 1 {
		t.Fatalf("expected 1 record while installed, got %d", len(rec.all()))
	}
	SetDecisionRecorder(nil)
	recordDecision(Decision{Kind: DecisionAutopass, Allow: false})
	if len(rec.all()) != 1 {
		t.Errorf("expected still 1 record after removal, got %d", len(rec.all()))
	}
}
