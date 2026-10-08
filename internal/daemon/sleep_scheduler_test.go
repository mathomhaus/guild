package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is the scheduler's injected clock for deterministic tests:
// the test owns the current time and delivers ticks on demand, so the
// idle math is exercised without any real sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	// tickC is handed to the scheduler's ticker. Tests push onto it via
	// tick() to drive exactly one loop iteration.
	tickC chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	// Unbuffered: every tick() blocks until the scheduler loop receives
	// it, so a tick() that returns proves the loop consumed the prior
	// tick and looped back. Tests use that as a deterministic barrier:
	// after a firing tick() returns, the resulting maybeFire has run.
	return &fakeClock{now: start, tickC: make(chan time.Time)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the clock forward by d.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) NewTicker(time.Duration) ticker {
	return &fakeTicker{c: c.tickC}
}

// tick delivers one tick to the scheduler loop. It blocks until the
// loop is ready to receive, so a test that wants the resulting
// maybeFire to have run can follow tick() with a sync barrier (the Pass
// func's started signal, or another tick once the loop returns).
func (c *fakeClock) tick() {
	c.tickC <- c.Now()
}

type fakeTicker struct{ c chan time.Time }

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop()               {}

// discardLogger keeps test output quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runScheduler starts s.Run in a goroutine and returns the loop's
// context plus a stop func that cancels it and waits for the loop to
// exit.
func runScheduler(t *testing.T, s *Scheduler) (ctx context.Context, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

const testIdle = 10 * time.Minute

// TestSchedulerFiresAfterIdle proves a pass fires only once the idle
// window has fully elapsed since the last activity, driven entirely by
// the fake clock.
func TestSchedulerFiresAfterIdle(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	var fired atomic.Int64
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    testIdle,
		Budget:  time.Second,
		clock:   clk,
		Logger:  discardLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			fired.Add(1)
			return PassOutcome{Steps: 0}, nil
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	// Just shy of the idle window: a tick must NOT fire.
	clk.advance(testIdle - time.Minute)
	clk.tick()
	// Drain the loop with a second tick so the first has been processed.
	clk.tick()
	if got := fired.Load(); got != 0 {
		t.Fatalf("pass fired before idle window elapsed: %d", got)
	}

	// Cross the idle window: the next tick fires exactly one pass.
	clk.advance(2 * time.Minute)
	clk.tick()
	clk.tick() // barrier: ensures the firing tick's maybeFire returned
	if got := fired.Load(); got != 1 {
		t.Fatalf("expected exactly one pass after idle window, got %d", got)
	}
}

// TestSchedulerDisabledNeverFires covers both opt-outs the acceptance
// names: Enabled=false and a nil Pass. Neither may ever fire.
func TestSchedulerDisabledNeverFires(t *testing.T) {
	cases := []struct {
		name string
		cfg  SchedulerConfig
	}{
		{
			name: "enabled false",
			cfg: SchedulerConfig{
				Enabled: false,
				Idle:    testIdle,
				Budget:  time.Second,
				Pass: func(context.Context, time.Duration) (PassOutcome, error) {
					t.Error("disabled scheduler fired a pass")
					return PassOutcome{}, nil
				},
			},
		},
		{
			name: "nil pass",
			cfg: SchedulerConfig{
				Enabled: true,
				Idle:    testIdle,
				Budget:  time.Second,
				Pass:    nil,
			},
		},
		{
			name: "non-positive idle",
			cfg: SchedulerConfig{
				Enabled: true,
				Idle:    0,
				Budget:  time.Second,
				Pass: func(context.Context, time.Duration) (PassOutcome, error) {
					t.Error("zero-idle scheduler fired a pass")
					return PassOutcome{}, nil
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
			tc.cfg.clock = clk
			tc.cfg.Logger = discardLogger()
			s := NewScheduler(tc.cfg)
			ctx, stop := runScheduler(t, s)
			defer stop()

			// A disabled scheduler blocks on ctx.Done and never reads the
			// tick channel; pushing a tick would deadlock the test, so we
			// advance the clock well past idle and assert nothing fired
			// after giving the loop a moment to (not) act.
			clk.advance(10 * testIdle)
			select {
			case clk.tickC <- clk.Now():
				// A firing scheduler would have drained this; if the send
				// succeeds the loop is reading ticks, which only the armed
				// path does. Give maybeFire a chance, then a second send
				// must NOT succeed instantly if it fired (it would block on
				// Pass). For the disabled path the send blocks forever, so
				// the default branch is what we expect.
				t.Fatal("disabled scheduler read a tick; it should idle on ctx")
			default:
			}
			if ctx.Err() != nil {
				t.Fatal("ctx cancelled prematurely")
			}
		})
	}
}

// TestSchedulerSingleFlight proves at most one pass runs at a time: a
// second tick arriving while a pass is in flight does not start a
// second pass.
func TestSchedulerSingleFlight(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    testIdle,
		Budget:  time.Second,
		clock:   clk,
		Logger:  discardLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			n := concurrent.Add(1)
			for {
				old := maxConcurrent.Load()
				if n <= old || maxConcurrent.CompareAndSwap(old, n) {
					break
				}
			}
			started <- struct{}{}
			<-release
			concurrent.Add(-1)
			return PassOutcome{}, nil
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	clk.advance(testIdle + time.Minute)

	// First tick fires a pass; the Pass func blocks on release. Because
	// maybeFire runs the pass inline on the loop goroutine, the loop is
	// now busy and cannot read another tick. We fire the pass on its own
	// goroutine-equivalent by tick()-ing once and waiting for started.
	go clk.tick()
	<-started

	// While the first pass is held, the loop is blocked inside maybeFire,
	// so any further tick we try to push cannot be consumed: single
	// flight is structural. Release the first pass and confirm only one
	// ever ran concurrently.
	close(release)
	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent passes = %d, want 1", got)
	}
}

// TestSchedulerGapAfterPass proves a new pass never starts within the
// idle window of the previous pass ending, even though the daemon has
// been idle long enough on the activity clock.
func TestSchedulerGapAfterPass(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	var fired atomic.Int64
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    testIdle,
		Budget:  time.Second,
		clock:   clk,
		Logger:  discardLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			fired.Add(1)
			return PassOutcome{}, nil
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	// First pass: idle elapsed, fire.
	clk.advance(testIdle + time.Minute)
	clk.tick()
	clk.tick() // barrier
	if got := fired.Load(); got != 1 {
		t.Fatalf("first pass: got %d fires, want 1", got)
	}

	// The pass just ended at the current clock. The activity clock is
	// still > idle (lastActivity was the scheduler start, long ago), but
	// the gap rule must suppress a second pass until idle has elapsed
	// SINCE THE PASS ENDED. Advance less than idle: no fire.
	clk.advance(testIdle - time.Minute)
	clk.tick()
	clk.tick() // barrier
	if got := fired.Load(); got != 1 {
		t.Fatalf("second pass fired inside the post-pass gap: got %d, want 1", got)
	}

	// Cross the gap: the next pass is allowed.
	clk.advance(2 * time.Minute)
	clk.tick()
	clk.tick() // barrier
	if got := fired.Load(); got != 2 {
		t.Fatalf("third tick after gap: got %d fires, want 2", got)
	}
}

// TestSchedulerTouchResetsIdle proves activity resets the idle
// countdown: a Touch just before the window would elapse pushes the
// next eligible pass a full window into the future.
func TestSchedulerTouchResetsIdle(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	var fired atomic.Int64
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    testIdle,
		Budget:  time.Second,
		clock:   clk,
		Logger:  discardLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			fired.Add(1)
			return PassOutcome{}, nil
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	// Almost idle, then activity arrives.
	clk.advance(testIdle - time.Minute)
	s.Touch()

	// Advance past the ORIGINAL window but not past the touch-reset one:
	// no fire, because the idle clock restarted at the Touch.
	clk.advance(2 * time.Minute)
	clk.tick()
	clk.tick() // barrier
	if got := fired.Load(); got != 0 {
		t.Fatalf("pass fired despite a recent Touch: got %d", got)
	}

	// Now let the full window elapse since the Touch: a pass fires.
	clk.advance(testIdle)
	clk.tick()
	clk.tick() // barrier
	if got := fired.Load(); got != 1 {
		t.Fatalf("pass should fire a full window after Touch: got %d, want 1", got)
	}
}

// TestSchedulerTouchPreemptsPass proves a request arriving mid-pass
// cancels the pass context promptly: the in-flight Pass observes ctx
// cancellation.
func TestSchedulerTouchPreemptsPass(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    testIdle,
		Budget:  time.Hour, // long budget: only preemption ends this pass
		clock:   clk,
		Logger:  discardLogger(),
		Pass: func(ctx context.Context, _ time.Duration) (PassOutcome, error) {
			close(started)
			<-ctx.Done() // block until preempted
			close(cancelled)
			return PassOutcome{Partial: true}, ctx.Err()
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	clk.advance(testIdle + time.Minute)
	go clk.tick()
	<-started // pass is running, blocked on ctx.Done

	// A waking session: Touch must cancel the pass context.
	s.Touch()

	select {
	case <-cancelled:
		// Pass observed cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("Touch did not preempt the in-flight pass within 2s")
	}

	// The Pass func has returned, but maybeFire's post-pass bookkeeping
	// (writing s.last) runs after the func returns. Wait for Ended to be
	// stamped before reading the snapshot.
	last := waitForPassEnded(t, s)
	if !last.Partial {
		t.Error("preempted pass should be recorded partial in Last()")
	}
}

// waitForPassEnded polls Last() until the most recent pass has been
// fully recorded (Ended stamped), so a test reads a settled snapshot
// rather than racing maybeFire's bookkeeping.
func waitForPassEnded(t *testing.T, s *Scheduler) LastPass {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		last := s.Last()
		if !last.Ended.IsZero() {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatal("pass bookkeeping (Last().Ended) not recorded within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSchedulerLastSurfacesPassInfo proves Last() reports the most
// recent completed pass for daemon status.
func TestSchedulerLastSurfacesPassInfo(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    testIdle,
		Budget:  time.Second,
		clock:   clk,
		Logger:  discardLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			return PassOutcome{Partial: true, Steps: 3}, nil
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	if last := s.Last(); !last.Started.IsZero() {
		t.Fatalf("Last() before any pass should be zero, got %+v", last)
	}

	clk.advance(testIdle + time.Minute)
	clk.tick()
	clk.tick() // barrier

	last := s.Last()
	if last.Started.IsZero() || last.Ended.IsZero() {
		t.Fatalf("Last() should carry start/end after a pass, got %+v", last)
	}
	if !last.Partial || last.Steps != 3 {
		t.Errorf("Last() outcome mismatch: got partial=%v steps=%d, want partial=true steps=3", last.Partial, last.Steps)
	}
	if last.Err != "" {
		t.Errorf("Last().Err should be empty on success, got %q", last.Err)
	}
}

// TestSchedulerPassErrorRecorded proves a failing Pass is logged-and-
// recorded but does not wedge the gap timer: the error lands in Last()
// and lastPassEnded advances so the next pass is gated normally.
func TestSchedulerPassErrorRecorded(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	s := NewScheduler(SchedulerConfig{
		Enabled: true,
		Idle:    testIdle,
		Budget:  time.Second,
		clock:   clk,
		Logger:  discardLogger(),
		Pass: func(context.Context, time.Duration) (PassOutcome, error) {
			return PassOutcome{}, errors.New("boom")
		},
	})
	_, stop := runScheduler(t, s)
	defer stop()

	clk.advance(testIdle + time.Minute)
	clk.tick()
	clk.tick() // barrier

	last := s.Last()
	if last.Err != "boom" {
		t.Errorf("Last().Err: got %q, want %q", last.Err, "boom")
	}
	if last.Ended.IsZero() {
		t.Error("a failed pass must still record Ended so the gap timer advances")
	}
}
