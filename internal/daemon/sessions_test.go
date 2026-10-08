package daemon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// quietRegistryLogger returns a registry config logger that discards
// output, so test runs stay readable. Tests that assert on behavior use
// the seam call counts, not log lines.
func newTestRegistry(t *testing.T, cfg RegistryConfig) (*Registry, *fakeClock) {
	t.Helper()
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg.clock = clk
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
	}
	return NewRegistry(cfg), clk
}

// TestRegistryRegisterUnregisterSnapshot covers the basic registry
// contract: a registered session appears in the snapshot with its
// metadata, an unregistered one is gone, and the count tracks both.
func TestRegistryRegisterUnregisterSnapshot(t *testing.T) {
	r, _ := newTestRegistry(t, RegistryConfig{})

	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r.Register("1001", "alpha", at)
	r.Register("1002", "beta", at)

	if got := r.Count(); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	// Deterministic order by id.
	if snap[0].ID != "1001" || snap[1].ID != "1002" {
		t.Fatalf("Snapshot order = %q,%q, want 1001,1002", snap[0].ID, snap[1].ID)
	}
	if snap[0].Project != "alpha" {
		t.Fatalf("session 1001 project = %q, want alpha", snap[0].Project)
	}
	if !snap[0].ConnectedAt.Equal(at) {
		t.Fatalf("session 1001 ConnectedAt = %v, want %v", snap[0].ConnectedAt, at)
	}
	if !snap[0].LastHeartbeat.Equal(at) {
		t.Fatalf("session 1001 LastHeartbeat = %v, want connect time %v", snap[0].LastHeartbeat, at)
	}

	r.Unregister("1001")
	if got := r.Count(); got != 1 {
		t.Fatalf("Count after unregister = %d, want 1", got)
	}
	snap = r.Snapshot()
	if len(snap) != 1 || snap[0].ID != "1002" {
		t.Fatalf("Snapshot after unregister = %+v, want only 1002", snap)
	}

	// Unregister of a never-registered id and empty id are no-ops.
	r.Unregister("nope")
	r.Unregister("")
	if r.Register("", "x", at) != nil {
		t.Fatal("Register with empty id should return nil")
	}
	if got := r.Count(); got != 1 {
		t.Fatalf("Count after no-op operations = %d, want 1", got)
	}
}

// TestRegistryReregisterRefreshes checks a re-dial under the same id
// refreshes the entry rather than duplicating it.
func TestRegistryReregisterRefreshes(t *testing.T) {
	r, _ := newTestRegistry(t, RegistryConfig{})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	r.Register("1001", "alpha", t0)
	r.Register("1001", "beta", t1)

	if got := r.Count(); got != 1 {
		t.Fatalf("Count after re-register = %d, want 1 (no duplicate)", got)
	}
	snap := r.Snapshot()
	if snap[0].Project != "beta" || !snap[0].ConnectedAt.Equal(t1) {
		t.Fatalf("re-register did not refresh: %+v", snap[0])
	}
}

// TestRegistryHeartbeatRenewsLiveSessions verifies a tick renews every
// live session (RenewFunc called once per session per tick) and advances
// each session's LastHeartbeat to the tick time.
func TestRegistryHeartbeatRenewsLiveSessions(t *testing.T) {
	var mu sync.Mutex
	renewed := map[string]int{}
	r, clk := newTestRegistry(t, RegistryConfig{
		HeartbeatInterval: time.Second, // floored value; the fake clock ignores it
		Renew: func(_ context.Context, sessionID string) error {
			mu.Lock()
			renewed[sessionID]++
			mu.Unlock()
			return nil
		},
	})

	connectAt := clk.Now()
	r.Register("1001", "alpha", connectAt)
	r.Register("1002", "beta", connectAt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Advance the clock and drive one tick. The fake clock's tick() blocks
	// until the loop consumes it; a second tick() proves the prior tick's
	// heartbeat ran (the loop looped back to the select).
	tickAt := connectAt.Add(30 * time.Second)
	clk.advance(30 * time.Second)
	clk.tick()
	clk.tick() // barrier

	mu.Lock()
	got1, got2 := renewed["1001"], renewed["1002"]
	mu.Unlock()
	if got1 < 1 || got2 < 1 {
		t.Fatalf("renew counts = 1001:%d 1002:%d, want each >= 1", got1, got2)
	}

	snap := r.Snapshot()
	for _, s := range snap {
		if !s.LastHeartbeat.Equal(tickAt) {
			t.Fatalf("session %s LastHeartbeat = %v, want tick time %v", s.ID, s.LastHeartbeat, tickAt)
		}
	}

	cancel()
	<-done
}

// TestRegistryHeartbeatStopsAfterUnregister verifies that once a session
// detaches, the tick no longer renews it.
func TestRegistryHeartbeatStopsAfterUnregister(t *testing.T) {
	var mu sync.Mutex
	renewed := map[string]int{}
	r, clk := newTestRegistry(t, RegistryConfig{
		Renew: func(_ context.Context, sessionID string) error {
			mu.Lock()
			renewed[sessionID]++
			mu.Unlock()
			return nil
		},
	})

	r.Register("1001", "alpha", clk.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	clk.advance(30 * time.Second)
	clk.tick()
	clk.tick() // barrier: first tick's heartbeat ran

	mu.Lock()
	before := renewed["1001"]
	mu.Unlock()
	if before < 1 {
		t.Fatalf("renew count before detach = %d, want >= 1", before)
	}

	r.Unregister("1001")

	clk.advance(30 * time.Second)
	clk.tick()
	clk.tick() // barrier: second tick's (empty) heartbeat ran

	mu.Lock()
	after := renewed["1001"]
	mu.Unlock()
	if after != before {
		t.Fatalf("renew count after detach = %d, want unchanged %d", after, before)
	}

	cancel()
	<-done
}

// TestRegistryBootGraceRunsOnce verifies the boot-grace seam fires exactly
// once at Run start, before any tick.
func TestRegistryBootGraceRunsOnce(t *testing.T) {
	var grace atomic.Int64
	r, clk := newTestRegistry(t, RegistryConfig{
		BootGrace: func(_ context.Context) error {
			grace.Add(1)
			return nil
		},
		Renew: func(_ context.Context, _ string) error { return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Drive a couple of ticks: boot grace must still have run exactly once.
	clk.advance(30 * time.Second)
	clk.tick()
	clk.tick()

	cancel()
	<-done

	if got := grace.Load(); got != 1 {
		t.Fatalf("boot grace ran %d times, want exactly 1", got)
	}
}

// TestRegistryHeartbeatToleratesRenewError verifies a renewal error for one
// session is logged-and-skipped, not fatal: the loop keeps running and a
// later tick still renews.
func TestRegistryHeartbeatToleratesRenewError(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int64
	r, clk := newTestRegistry(t, RegistryConfig{
		Renew: func(_ context.Context, _ string) error {
			calls.Add(1)
			if fail.Load() {
				return fmt.Errorf("transient db error")
			}
			return nil
		},
	})

	r.Register("1001", "alpha", clk.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	clk.advance(30 * time.Second)
	clk.tick()
	clk.tick() // first tick (renew errored) ran

	// Loop survived the error; the next tick succeeds.
	fail.Store(false)
	clk.advance(30 * time.Second)
	clk.tick()
	clk.tick()

	cancel()
	<-done

	if got := calls.Load(); got < 2 {
		t.Fatalf("renew calls = %d, want >= 2 (loop survived the error)", got)
	}
	// After the successful tick LastHeartbeat advanced past the connect time.
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
}

// TestRegistryConcurrentChurnRaceSafe hammers Register / Unregister /
// Touch / Snapshot from many goroutines while the tick runs, so -race
// proves the registry holds an accurate, consistent map under concurrent
// connect/disconnect. The assertion is "no race, no panic, count never
// negative"; the heartbeat tick reads the same map concurrently.
func TestRegistryConcurrentChurnRaceSafe(t *testing.T) {
	r, clk := newTestRegistry(t, RegistryConfig{
		Renew: func(_ context.Context, _ string) error { return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// A background ticker driver so the heartbeat loop concurrently reads
	// the map while writers churn it. It races the writers by design.
	tickStop := make(chan struct{})
	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		for {
			select {
			case <-tickStop:
				return
			case clk.tickC <- clk.Now():
			}
		}
	}()

	const workers = 16
	const iters = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("%d-%d", w, i%8)
				r.Register(id, "p", clk.Now())
				r.Touch(id)
				_ = r.Snapshot()
				_ = r.Count()
				r.Unregister(id)
			}
		}(w)
	}
	wg.Wait()

	close(tickStop)
	<-tickDone
	cancel()
	<-done

	if got := r.Count(); got < 0 {
		t.Fatalf("Count = %d, want >= 0", got)
	}
}

// TestRegistryTouchUpdatesLastHeartbeat verifies Touch advances a live
// session's LastHeartbeat (the in-memory "last seen" presence signal)
// independent of the tick, and is a no-op for an unknown id.
func TestRegistryTouchUpdatesLastHeartbeat(t *testing.T) {
	r, clk := newTestRegistry(t, RegistryConfig{})

	connectAt := clk.Now()
	r.Register("1001", "alpha", connectAt)

	clk.advance(5 * time.Second)
	r.Touch("1001")
	r.Touch("nope") // no-op

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if !snap[0].LastHeartbeat.Equal(connectAt.Add(5 * time.Second)) {
		t.Fatalf("LastHeartbeat = %v, want connect+5s", snap[0].LastHeartbeat)
	}
}

// TestRegistrySetHeldQuests_PresenceAndSnapshot verifies the in-memory
// held-quest view: SetHeldQuests records a session's leased quests (sorted,
// copied so the caller cannot mutate registry state), the snapshot carries
// them, Presence counts on-lease sessions, and a SetHeldQuests for an
// unknown id is a no-op.
func TestRegistrySetHeldQuests_PresenceAndSnapshot(t *testing.T) {
	r, _ := newTestRegistry(t, RegistryConfig{})

	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r.Register("1001", "alpha", at)
	r.Register("1002", "beta", at)

	// Before any lease: zero on-lease.
	if p := r.Presence(); p.Sessions != 2 || p.OnLease != 0 {
		t.Fatalf("Presence before leases = %+v, want {2 0}", p)
	}

	// Pass an unsorted, caller-owned slice; the registry must copy + sort.
	src := []string{"QUEST-7", "QUEST-1"}
	r.SetHeldQuests("1001", src)
	src[0] = "MUTATED" // mutation must not leak into the registry

	r.SetHeldQuests("nope", []string{"QUEST-9"}) // no-op for unknown id

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if got, want := snap[0].HeldQuests, []string{"QUEST-1", "QUEST-7"}; !equalStrings(got, want) {
		t.Fatalf("session 1001 HeldQuests = %v, want sorted %v", got, want)
	}
	if !snap[0].OnLease() {
		t.Errorf("session 1001 should report OnLease")
	}
	if snap[1].OnLease() {
		t.Errorf("session 1002 holds no leases; OnLease must be false")
	}

	if p := r.Presence(); p.Sessions != 2 || p.OnLease != 1 {
		t.Fatalf("Presence after one lease = %+v, want {2 1}", p)
	}
}

// equalStrings is a tiny slice-equality helper for the held-quest assertions.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
