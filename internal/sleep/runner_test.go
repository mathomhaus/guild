package sleep

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// fakeStep is a configurable Step for runner tests. ran flips when Run
// is invoked so tests can assert skip semantics.
type fakeStep struct {
	name string
	run  func(ctx context.Context, pc *PassContext) (StepReport, error)
	ran  bool
}

func (s *fakeStep) Name() string { return s.name }

func (s *fakeStep) Run(ctx context.Context, pc *PassContext) (StepReport, error) {
	s.ran = true
	return s.run(ctx, pc)
}

// quietLogger keeps runner diagnostics out of test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRun_BudgetOverrun locks the wall-budget contract: a step that
// exceeds the budget is cancelled via the context deadline, the pass
// still gets ended_at, the overrunning step is journaled as partial,
// and the remaining steps are skipped.
func TestRun_BudgetOverrun(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	pc := &PassContext{LoreDB: db, Trigger: TriggerDaemonIdle, Logger: quietLogger()}

	overrunner := &fakeStep{
		name: "overrunner",
		run: func(ctx context.Context, _ *PassContext) (StepReport, error) {
			// Simulate work that never finishes inside the budget: the
			// only way out is the deadline firing.
			<-ctx.Done()
			return StepReport{}, ctx.Err()
		},
	}
	never := &fakeStep{
		name: "never",
		run: func(_ context.Context, _ *PassContext) (StepReport, error) {
			return StepReport{}, nil
		},
	}

	res, err := Run(ctx, pc, []Step{overrunner, never}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Partial {
		t.Errorf("result.Partial = false, want true")
	}
	if len(res.Steps) != 2 {
		t.Fatalf("len(res.Steps) = %d, want 2", len(res.Steps))
	}
	if !res.Steps[0].Partial {
		t.Errorf("overrunning step not marked partial: %+v", res.Steps[0])
	}
	if !errors.Is(res.Steps[0].Err, context.DeadlineExceeded) {
		t.Errorf("overrunning step err = %v, want context.DeadlineExceeded", res.Steps[0].Err)
	}
	if !res.Steps[1].Skipped || never.ran {
		t.Errorf("second step: skipped=%v ran=%v, want skipped and never run",
			res.Steps[1].Skipped, never.ran)
	}

	// The pass row must carry ended_at despite the expired budget.
	passes, err := UnnarratedPasses(ctx, db)
	if err != nil {
		t.Fatalf("UnnarratedPasses: %v", err)
	}
	if len(passes) != 1 || passes[0].ID != res.PassID {
		t.Fatalf("UnnarratedPasses = %+v, want the single ended pass %d", passes, res.PassID)
	}
	if passes[0].EndedAt.IsZero() {
		t.Errorf("pass ended_at is zero, want set")
	}

	// The overrunning step is journaled as partial.
	ops, err := PassOps(ctx, db, res.PassID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("len(ops) = %d, want 1 (the partial marker)", len(ops))
	}
	marker := ops[0]
	if marker.Kind != opStepPartial || marker.Step != "overrunner" {
		t.Errorf("marker = (%q, %q), want (step_partial, overrunner)", marker.Kind, marker.Step)
	}
	if marker.Applied {
		t.Errorf("marker.Applied = true, want false (bookkeeping row, no mutation)")
	}
}

// TestRun_FailingStepDoesNotAbortOthers locks per-step failure
// isolation: a step error is journaled and reported, and the next step
// still runs (and can journal real ops against the live PassID).
func TestRun_FailingStepDoesNotAbortOthers(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	pc := &PassContext{LoreDB: db, Trigger: TriggerAutopass, Logger: quietLogger()}

	boom := &fakeStep{
		name: "boom",
		run: func(_ context.Context, _ *PassContext) (StepReport, error) {
			return StepReport{}, errors.New("synthetic step failure")
		},
	}
	worker := &fakeStep{
		name: "worker",
		run: func(ctx context.Context, pc *PassContext) (StepReport, error) {
			// A real step journals its mutations against pc.PassID; do
			// the same so the test proves PassID threading end to end.
			_, err := RecordOp(ctx, pc.LoreDB, Op{
				PassID:  pc.PassID,
				Step:    "worker",
				Policy:  PolicyAuto,
				Kind:    OpEmbedBackfill,
				Target:  "LORE-7",
				Detail:  `{"vectors":1}`,
				Applied: true,
			})
			if err != nil {
				return StepReport{}, err
			}
			return StepReport{OpsApplied: 1}, nil
		},
	}

	res, err := Run(ctx, pc, []Step{boom, worker}, 5*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Partial {
		t.Errorf("result.Partial = true, want false (failure is not budget expiry)")
	}
	if len(res.Steps) != 2 {
		t.Fatalf("len(res.Steps) = %d, want 2", len(res.Steps))
	}
	if res.Steps[0].Err == nil || res.Steps[0].Partial || res.Steps[0].Skipped {
		t.Errorf("boom result = %+v, want plain error", res.Steps[0])
	}
	if !worker.ran || res.Steps[1].Err != nil {
		t.Errorf("worker: ran=%v err=%v, want ran with nil error", worker.ran, res.Steps[1].Err)
	}
	if res.Steps[1].Report.OpsApplied != 1 {
		t.Errorf("worker OpsApplied = %d, want 1", res.Steps[1].Report.OpsApplied)
	}

	ops, err := PassOps(ctx, db, res.PassID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("len(ops) = %d, want 2 (error marker + worker op)", len(ops))
	}
	if ops[0].Kind != opStepError || ops[0].Step != "boom" || ops[0].Applied {
		t.Errorf("ops[0] = %+v, want unapplied step_error for boom", ops[0])
	}
	if ops[1].Kind != OpEmbedBackfill || ops[1].Step != "worker" || !ops[1].Applied {
		t.Errorf("ops[1] = %+v, want applied embed_backfill for worker", ops[1])
	}

	if passes, err := UnnarratedPasses(ctx, db); err != nil || len(passes) != 1 || passes[0].EndedAt.IsZero() {
		t.Fatalf("UnnarratedPasses = %+v, %v; want one ended pass", passes, err)
	}
}

// TestRun_PanickingStepIsIsolated proves a panicking step degrades to
// a step error instead of taking down the caller (the daemon, in
// production).
func TestRun_PanickingStepIsIsolated(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	pc := &PassContext{LoreDB: db, Trigger: TriggerDaemonIdle, Logger: quietLogger()}

	panicker := &fakeStep{
		name: "panicker",
		run: func(_ context.Context, _ *PassContext) (StepReport, error) {
			panic("synthetic panic")
		},
	}
	after := &fakeStep{
		name: "after",
		run: func(_ context.Context, _ *PassContext) (StepReport, error) {
			return StepReport{}, nil
		},
	}

	res, err := Run(ctx, pc, []Step{panicker, after}, 5*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps[0].Err == nil {
		t.Errorf("panicking step err = nil, want panic converted to error")
	}
	if !after.ran {
		t.Errorf("step after panicker did not run")
	}
}

// TestRun_Validation locks the substrate-error paths.
func TestRun_Validation(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)

	if _, err := Run(ctx, nil, nil, time.Second); err == nil {
		t.Errorf("Run(nil pc): want error, got nil")
	}
	if _, err := Run(ctx, &PassContext{Trigger: TriggerDaemonIdle}, nil, time.Second); err == nil {
		t.Errorf("Run(nil LoreDB): want error, got nil")
	}
	if _, err := Run(ctx, &PassContext{LoreDB: db, Trigger: TriggerDaemonIdle}, nil, 0); err == nil {
		t.Errorf("Run(zero budget): want error, got nil")
	}
	if _, err := Run(ctx, &PassContext{LoreDB: db, Trigger: Trigger("cron")}, nil, time.Second); err == nil {
		t.Errorf("Run(bad trigger): want error, got nil")
	}
}
