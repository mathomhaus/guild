package sleep

import (
	"context"
	"testing"
	"time"
)

// swapRegistryForTest replaces the process-global step registry with
// steps for the duration of the test and restores it in t.Cleanup. It
// touches the unexported registry directly (same package) rather than
// the production RegisterStep path so a test can drive Autopass with a
// known step set without leaking registrations into sibling tests.
func swapRegistryForTest(t *testing.T, steps ...Step) {
	t.Helper()
	registryMu.Lock()
	prev := registry
	registry = steps
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = prev
		registryMu.Unlock()
	})
}

// TestAutopass_RunsWhenNoPriorPass proves the happy path: with an empty
// journal there is nothing to throttle against, so Autopass runs a pass
// and records it with trigger 'autopass'.
func TestAutopass_RunsWhenNoPriorPass(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)

	res, ran, err := Autopass(ctx, AutopassConfig{
		LoreDB:      db,
		Budget:      time.Second,
		MinInterval: 6 * time.Hour,
		Logger:      quietLogger(),
		Now:         func() time.Time { return time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Autopass: %v", err)
	}
	if !ran {
		t.Fatalf("ran = false, want true (empty journal must not throttle)")
	}
	if res == nil {
		t.Fatalf("res = nil, want a pass result")
	}

	passes, err := UnnarratedPasses(ctx, db)
	if err != nil {
		t.Fatalf("UnnarratedPasses: %v", err)
	}
	if len(passes) != 1 {
		t.Fatalf("len(passes) = %d, want 1", len(passes))
	}
	if passes[0].Trigger != TriggerAutopass {
		t.Errorf("trigger = %q, want %q", passes[0].Trigger, TriggerAutopass)
	}
}

// TestAutopass_ThrottledWhenRecent proves the throttle: a pass that
// ended within MinInterval skips the autopass entirely, so a
// briefly-stopped daemon does not cause duplicate work.
func TestAutopass_ThrottledWhenRecent(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)

	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	// Seed a pass that ended one hour ago, inside a six-hour window.
	id, err := BeginPass(ctx, db, TriggerDaemonIdle, time.Minute, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	if err := EndPass(ctx, db, id, now.Add(-time.Hour)); err != nil {
		t.Fatalf("EndPass: %v", err)
	}

	res, ran, err := Autopass(ctx, AutopassConfig{
		LoreDB:      db,
		Budget:      time.Second,
		MinInterval: 6 * time.Hour,
		Logger:      quietLogger(),
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Autopass: %v", err)
	}
	if ran {
		t.Errorf("ran = true, want false (recent pass must throttle)")
	}
	if res != nil {
		t.Errorf("res = %+v, want nil on throttle", res)
	}

	// The journal must still hold exactly the one seeded pass: no new
	// pass row was written.
	passes, err := UnnarratedPasses(ctx, db)
	if err != nil {
		t.Fatalf("UnnarratedPasses: %v", err)
	}
	if len(passes) != 1 || passes[0].ID != id {
		t.Fatalf("passes = %+v, want only the seeded pass %d", passes, id)
	}
}

// TestAutopass_RunsWhenStale proves the throttle boundary: a pass that
// ended longer ago than MinInterval no longer suppresses a new autopass.
func TestAutopass_RunsWhenStale(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)

	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	// Seed a pass that ended seven hours ago, outside a six-hour window.
	id, err := BeginPass(ctx, db, TriggerAutopass, time.Minute, now.Add(-8*time.Hour))
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	if err := EndPass(ctx, db, id, now.Add(-7*time.Hour)); err != nil {
		t.Fatalf("EndPass: %v", err)
	}

	_, ran, err := Autopass(ctx, AutopassConfig{
		LoreDB:      db,
		Budget:      time.Second,
		MinInterval: 6 * time.Hour,
		Logger:      quietLogger(),
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Autopass: %v", err)
	}
	if !ran {
		t.Errorf("ran = false, want true (stale pass must not throttle)")
	}
}

// TestAutopass_RunsRegisteredSteps proves a registered step actually
// runs under the autopass with the configured caps threaded through, and
// that the trigger and caps reach the step's PassContext.
func TestAutopass_RunsRegisteredSteps(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)

	var gotTrigger Trigger
	var gotCaps Caps
	probe := &fakeStep{
		name: "probe",
		run: func(_ context.Context, pc *PassContext) (StepReport, error) {
			gotTrigger = pc.Trigger
			gotCaps = pc.Caps
			return StepReport{Note: "ok"}, nil
		},
	}
	swapRegistryForTest(t, probe)

	caps := Caps{MaxAutoOps: 1, MaxQuestPosts: 1, MaxRenewalPosts: 1}
	res, ran, err := Autopass(ctx, AutopassConfig{
		LoreDB:      db,
		Budget:      time.Second,
		MinInterval: 0, // disable throttle for a deterministic single run
		Caps:        caps,
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("Autopass: %v", err)
	}
	if !ran {
		t.Fatalf("ran = false, want true")
	}
	if !probe.ran {
		t.Errorf("registered step did not run")
	}
	if gotTrigger != TriggerAutopass {
		t.Errorf("step saw trigger %q, want %q", gotTrigger, TriggerAutopass)
	}
	if gotCaps != caps {
		t.Errorf("step saw caps %+v, want %+v", gotCaps, caps)
	}
	if len(res.Steps) != 1 {
		t.Errorf("len(res.Steps) = %d, want 1", len(res.Steps))
	}
}

// TestAutopass_Validation locks the usage-error guards: a nil lore db or
// a non-positive budget is a hard error, not a silent no-op.
func TestAutopass_Validation(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)

	if _, ran, err := Autopass(ctx, AutopassConfig{Budget: time.Second}); err == nil || ran {
		t.Errorf("nil lore db: got ran=%v err=%v, want ran=false and an error", ran, err)
	}
	if _, ran, err := Autopass(ctx, AutopassConfig{LoreDB: db, Budget: 0}); err == nil || ran {
		t.Errorf("zero budget: got ran=%v err=%v, want ran=false and an error", ran, err)
	}
}
