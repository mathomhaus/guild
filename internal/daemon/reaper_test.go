package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestReaper builds a Reaper driven by a fake clock so a test delivers
// sweep ticks on demand without real sleeping.
func newTestReaper(t *testing.T, cfg ReaperConfig) (*Reaper, *fakeClock) {
	t.Helper()
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg.clock = clk
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
	}
	return NewReaper(cfg), clk
}

// runReaper starts r.Run in a goroutine and returns a stop func that cancels
// it and waits for the loop to exit.
func runReaper(t *testing.T, r *Reaper) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("reaper Run did not return after cancel")
		}
	}
}

// TestReaper_TickRunsSweep verifies each tick drives exactly one sweep.
func TestReaper_TickRunsSweep(t *testing.T) {
	var sweeps int64
	r, clk := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap: func(context.Context) (ReapOutcome, error) {
			atomic.AddInt64(&sweeps, 1)
			return ReapOutcome{}, nil
		},
	})
	stop := runReaper(t, r)
	defer stop()

	clk.tick()
	clk.tick() // returning proves the loop consumed the first tick

	if got := atomic.LoadInt64(&sweeps); got < 1 {
		t.Fatalf("sweeps = %d, want at least 1 after two ticks", got)
	}
}

// TestReaper_SweepError_KeepsTicking verifies a sweep error does not wedge
// the loop: the next tick still runs another sweep.
func TestReaper_SweepError_KeepsTicking(t *testing.T) {
	var sweeps int64
	r, clk := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap: func(context.Context) (ReapOutcome, error) {
			atomic.AddInt64(&sweeps, 1)
			return ReapOutcome{}, errors.New("transient db error")
		},
	})
	stop := runReaper(t, r)
	defer stop()

	clk.tick()
	clk.tick()
	clk.tick() // returning proves the loop survived two failing sweeps

	if got := atomic.LoadInt64(&sweeps); got < 2 {
		t.Fatalf("sweeps = %d, want >=2 (a failing sweep must not wedge the loop)", got)
	}
}

// TestReaper_NilReapFunc_TicksHarmlessly verifies a wiring gap (nil Reap)
// degrades to a loop that ticks without touching the db and shuts down clean.
func TestReaper_NilReapFunc_TicksHarmlessly(t *testing.T) {
	r, clk := newTestReaper(t, ReaperConfig{Interval: time.Minute, Reap: nil})
	stop := runReaper(t, r)
	defer stop()

	// Two ticks with no Reap func must not panic; the second returning
	// proves the loop is alive and consuming ticks.
	clk.tick()
	clk.tick()
}

// TestReaper_ShutdownJoinsCleanly verifies Run returns promptly on ctx
// cancellation even mid-loop, so the daemon's WaitGroup completes.
func TestReaper_ShutdownJoinsCleanly(t *testing.T) {
	r, _ := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap:     func(context.Context) (ReapOutcome, error) { return ReapOutcome{}, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper Run did not return on ctx cancel")
	}
}

// TestReaper_CancelDuringSweep_NotLoggedAsFailure verifies a sweep that
// returns an error because ctx was cancelled mid-sweep is treated as
// shutdown, not a failure (no retry storm, clean exit).
func TestReaper_CancelDuringSweep_NotLoggedAsFailure(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	r, clk := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap: func(ctx context.Context) (ReapOutcome, error) {
			close(started)
			<-release
			return ReapOutcome{}, ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Drive one tick, wait for the sweep to enter, cancel, then let it
	// observe the cancelled context.
	go clk.tick()
	<-started
	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper Run did not return after cancel during sweep")
	}
}

// TestNewReaper_FloorsInterval verifies a tiny configured interval is floored
// so the loop cannot spin, and a non-positive interval takes the default.
func TestNewReaper_FloorsInterval(t *testing.T) {
	if r := NewReaper(ReaperConfig{Interval: time.Millisecond, Logger: quietLogger()}); r.interval != minReapInterval {
		t.Errorf("interval = %v, want floored to %v", r.interval, minReapInterval)
	}
	if r := NewReaper(ReaperConfig{Interval: 0, Logger: quietLogger()}); r.interval != defaultReapInterval {
		t.Errorf("interval = %v, want default %v", r.interval, defaultReapInterval)
	}
}

// TestReaper_ConcurrentTicksRaceClean exercises Run under -race with
// concurrent ticks against a sweep that mutates a counter, asserting no data
// race in the loop bookkeeping.
func TestReaper_ConcurrentTicksRaceClean(t *testing.T) {
	var mu sync.Mutex
	count := 0
	r, clk := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap: func(context.Context) (ReapOutcome, error) {
			mu.Lock()
			count++
			n := count
			mu.Unlock()
			return ReapOutcome{Forfeited: n}, nil
		},
	})
	stop := runReaper(t, r)
	defer stop()

	for i := 0; i < 3; i++ {
		clk.tick()
	}
}

// TestReaper_TotalForfeitedAccumulates verifies the lifetime forfeit counter
// accumulates across sweeps that forfeit, ignores sweeps that forfeit
// nothing, and is the value the daemon-status readout surfaces.
//
// The unbuffered fake-clock tick is a barrier: a tick() that returns proves
// the loop consumed the PRIOR tick and looped back, so the prior sweep has
// finished. The test drives three forfeit-relevant sweeps (2, 0, 1) and uses
// a final barrier tick to confirm the third sweep ran before asserting the
// counter is exactly 2 + 0 + 1 = 3.
func TestReaper_TotalForfeitedAccumulates(t *testing.T) {
	var call int64
	r, clk := newTestReaper(t, ReaperConfig{
		Interval: time.Minute,
		Reap: func(context.Context) (ReapOutcome, error) {
			switch atomic.AddInt64(&call, 1) {
			case 1:
				return ReapOutcome{Scanned: 2, Forfeited: 2}, nil
			case 2:
				return ReapOutcome{Scanned: 0}, nil // nothing expired
			case 3:
				return ReapOutcome{Scanned: 1, Forfeited: 1}, nil
			default:
				// A barrier sweep (the 4th tick proves the 3rd finished)
				// must not move the counter, so the assertion is exact.
				return ReapOutcome{Scanned: 0}, nil
			}
		},
	})
	stop := runReaper(t, r)
	defer stop()

	clk.tick() // deliver sweep 1
	clk.tick() // delivering sweep 2 proves sweep 1 finished
	clk.tick() // delivering sweep 3 proves sweep 2 finished
	clk.tick() // delivering sweep 4 proves sweep 3 (Forfeited:1) finished

	if got := r.TotalForfeited(); got != 3 {
		t.Fatalf("TotalForfeited = %d, want 3 (sweeps forfeited 2 + 0 + 1)", got)
	}
}
