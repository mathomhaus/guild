package sleep

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
)

// Runner bookkeeping op kinds. Unexported on purpose: they are audit
// markers the runner writes about step outcomes, not mutations a step
// proposes, so they are not part of the defined OpKind taxonomy in
// policy.go (where unknown kinds default-deny to PolicyApproval).
// The rows carry policy='auto' (the runner records them unattended)
// and applied=0 (nothing mutated).
const (
	// opStepPartial marks a step that was cancelled by the pass wall
	// budget before it could finish.
	opStepPartial OpKind = "step_partial"

	// opStepError marks a step that failed for a non-budget reason.
	opStepError OpKind = "step_error"
)

// runnerOpTarget is the placeholder target for runner bookkeeping
// rows: they describe a step, not a LORE-N / QUEST-N entity.
const runnerOpTarget = "-"

// endPassGrace bounds the final EndPass write when the caller's
// context is already cancelled. The pass row must get ended_at even on
// budget expiry or daemon shutdown, so the write runs under
// context.WithoutCancel plus this timeout.
const endPassGrace = 5 * time.Second

// Caps carries per-pass guardrails for step implementations: how much
// unattended mutation one pass may do. Zero values mean "no cap".
// Enforcement belongs to the steps (only a step knows what one op
// costs); the runner just threads the caps through PassContext.
type Caps struct {
	// MaxAutoOps bounds how many PolicyAuto ops a single pass may
	// apply across all steps.
	MaxAutoOps int

	// MaxQuestPosts bounds how many quests (renewal + approval) a
	// single pass may post across all steps.
	MaxQuestPosts int

	// MaxRenewalPosts bounds how many lore-renewal quests the renewal
	// step may post in a single pass, across all projects. Stale entries
	// past the cap are journaled as overflow and picked up by a later
	// pass (oldest-first via the poster's deterministic selection). Zero
	// means "post nothing this pass"; the renewal step is the only
	// caller, so a zero default keeps an unconfigured pass inert rather
	// than flooding the board.
	MaxRenewalPosts int
}

// PassContext carries everything a step needs to do its work. The
// runner constructs the pass-scoped fields (PassID); the caller (the
// daemon idle scheduler or the in-process autopass) wires the rest.
//
// Writes go only through these db handles, so the daemon single-writer
// discipline holds when the daemon is the caller.
type PassContext struct {
	// LoreDB is the lore.db handle. Required: the sleep journal's
	// canonical home is lore.db, so the runner journals through it.
	LoreDB *sql.DB

	// QuestDB is the quest.db handle, for steps that post quests.
	// Optional; steps that need it must check for nil.
	QuestDB *sql.DB

	// Embed carries the embedding-pipeline dependencies for steps
	// that encode vectors. nil or disabled means BM25-only behavior,
	// same contract as everywhere else in internal/lore.
	Embed *lore.EmbedDeps

	// Logger receives structured diagnostics. nil defaults to
	// slog.Default().
	Logger *slog.Logger

	// Caps are the per-pass mutation guardrails steps must respect.
	Caps Caps

	// Now supplies timestamps, swappable for deterministic tests.
	// nil defaults to time.Now().UTC.
	Now func() time.Time

	// Trigger names what started this pass; recorded on the
	// sleep_passes row.
	Trigger Trigger

	// PassID is the sleep_passes row id for the running pass. Set by
	// Run after BeginPass; steps use it to RecordOp their mutations.
	PassID int64
}

// logger returns the configured logger or slog.Default().
func (pc *PassContext) logger() *slog.Logger {
	if pc.Logger != nil {
		return pc.Logger
	}
	return slog.Default()
}

// now returns the configured clock or time.Now().UTC.
func (pc *PassContext) now() time.Time {
	if pc.Now != nil {
		return pc.Now()
	}
	return time.Now().UTC()
}

// StepReport is what a step tells the runner about a completed run.
type StepReport struct {
	// OpsApplied counts PolicyAuto ops the step applied and journaled.
	OpsApplied int

	// OpsPosted counts quests the step posted (renewal posts plus
	// approval posts standing in for gated mutations).
	OpsPosted int

	// Note is an optional one-line human-readable summary for
	// narration.
	Note string
}

// Step is one unit of sleep-pass work (consolidation, echo renewal,
// embed backfill, ...). Implementations live in later quests; the
// runner treats them as opaque.
//
// Contract: respect ctx cancellation (the runner wires the pass wall
// budget through it), journal every mutation via RecordOp using
// pc.PassID, gate every mutation through Classify, and respect
// pc.Caps.
type Step interface {
	// Name identifies the step in journal rows and logs.
	Name() string

	// Run does the work. Returning an error marks the step failed but
	// does not abort the pass; the runner isolates per-step failures.
	Run(ctx context.Context, pc *PassContext) (StepReport, error)
}

// StepResult is the runner's record of one step's outcome.
type StepResult struct {
	// Name is the step's Name().
	Name string

	// Report is the step's own report (zero when the step failed or
	// was skipped).
	Report StepReport

	// Err is the step's failure, nil on success. Partial steps carry
	// the cancellation error they returned, if any.
	Err error

	// Partial is true when the pass wall budget cancelled the step
	// before it finished.
	Partial bool

	// Skipped is true when the step never ran because the budget was
	// already exhausted (or the caller's context was cancelled).
	Skipped bool
}

// PassResult is the runner's record of one whole pass.
type PassResult struct {
	// PassID is the sleep_passes row id.
	PassID int64

	// Partial is true when the wall budget (or caller cancellation)
	// ended the pass before every step completed.
	Partial bool

	// Steps holds one result per configured step, in execution order.
	Steps []StepResult
}

// Run executes steps sequentially under a wall budget and journals the
// pass. The budget is enforced via a deadline context handed to each
// step: a step that overruns is cancelled, journaled as partial, and
// the remaining steps are skipped. A step that fails for a non-budget
// reason is journaled as an error and does NOT abort the others.
//
// The pass row always gets ended_at, even on budget expiry or caller
// cancellation: the final EndPass runs under context.WithoutCancel
// with a small grace timeout.
//
// Run returns an error only for substrate failures (invalid arguments,
// BeginPass/EndPass failures). Step failures are reported per-step in
// PassResult.
func Run(ctx context.Context, pc *PassContext, steps []Step, budget time.Duration) (*PassResult, error) {
	if pc == nil {
		return nil, fmt.Errorf("sleep: run: nil pass context")
	}
	if pc.LoreDB == nil {
		return nil, fmt.Errorf("sleep: run: nil lore db")
	}
	if budget <= 0 {
		return nil, fmt.Errorf("sleep: run: non-positive budget %v", budget)
	}

	logger := pc.logger()

	passID, err := BeginPass(ctx, pc.LoreDB, pc.Trigger, budget, pc.now())
	if err != nil {
		return nil, fmt.Errorf("sleep: run: %w", err)
	}
	pc.PassID = passID

	logger.Info("sleep pass started",
		slog.Int64("pass_id", passID),
		slog.String("trigger", string(pc.Trigger)),
		slog.Duration("budget", budget),
		slog.Int("steps", len(steps)),
	)

	result := &PassResult{
		PassID: passID,
		Steps:  make([]StepResult, 0, len(steps)),
	}

	// The wall budget rides on a deadline context handed to each step.
	// Runner journal writes use the parent ctx instead, so the partial
	// record and EndPass still land after the budget expires.
	stepCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	for _, s := range steps {
		name := s.Name()

		if stepCtx.Err() != nil {
			// Budget exhausted (or caller cancelled) before this step
			// started: skip it. Nothing ran, so nothing is journaled.
			result.Partial = true
			result.Steps = append(result.Steps, StepResult{Name: name, Skipped: true})
			continue
		}

		report, stepErr := runStep(stepCtx, s, pc)

		switch {
		case stepErr != nil && errors.Is(stepCtx.Err(), context.DeadlineExceeded):
			// The wall budget cancelled the step mid-flight. Journal it
			// as partial so narration can say "pass ran out of budget
			// during <step>".
			result.Partial = true
			result.Steps = append(result.Steps, StepResult{Name: name, Err: stepErr, Partial: true})
			journalStepMarker(ctx, pc, name, opStepPartial, stepErr, budget, logger)
			logger.Warn("sleep step cut by wall budget",
				slog.Int64("pass_id", passID),
				slog.String("step", name),
				slog.Duration("budget", budget),
			)
		case stepErr != nil && ctx.Err() != nil:
			// The caller's context died (daemon shutdown, not budget).
			// Record the failure and stop; every later step would fail
			// the same way.
			result.Partial = true
			result.Steps = append(result.Steps, StepResult{Name: name, Err: stepErr})
			logger.Warn("sleep step aborted by caller cancellation",
				slog.Int64("pass_id", passID),
				slog.String("step", name),
				slog.String("err", stepErr.Error()),
			)
		case stepErr != nil:
			// Ordinary failure: journal, log, move on. One failing step
			// does not abort the others (mirrors the per-target
			// isolation in the embed auto-backfill trigger).
			result.Steps = append(result.Steps, StepResult{Name: name, Err: stepErr})
			journalStepMarker(ctx, pc, name, opStepError, stepErr, budget, logger)
			logger.Warn("sleep step failed",
				slog.Int64("pass_id", passID),
				slog.String("step", name),
				slog.String("err", stepErr.Error()),
			)
		default:
			result.Steps = append(result.Steps, StepResult{Name: name, Report: report})
			logger.Info("sleep step complete",
				slog.Int64("pass_id", passID),
				slog.String("step", name),
				slog.Int("ops_applied", report.OpsApplied),
				slog.Int("ops_posted", report.OpsPosted),
			)
		}
	}

	// The pass row must get ended_at no matter how the loop exited;
	// WithoutCancel shields the write from budget expiry and caller
	// cancellation, the grace timeout keeps it bounded.
	endCtx, endCancel := context.WithTimeout(context.WithoutCancel(ctx), endPassGrace)
	defer endCancel()
	if err := EndPass(endCtx, pc.LoreDB, passID, pc.now()); err != nil {
		return result, fmt.Errorf("sleep: run: %w", err)
	}

	logger.Info("sleep pass ended",
		slog.Int64("pass_id", passID),
		slog.Bool("partial", result.Partial),
	)
	return result, nil
}

// runStep invokes one step with panic isolation: a panicking step is
// converted into a step error so it cannot take down the daemon (or
// the host process running an autopass).
func runStep(ctx context.Context, s Step, pc *PassContext) (report StepReport, err error) {
	defer func() {
		if r := recover(); r != nil {
			report = StepReport{}
			err = fmt.Errorf("sleep: step %s panicked: %v", s.Name(), r)
		}
	}()
	return s.Run(ctx, pc)
}

// stepMarkerDetail is the JSON shape of runner bookkeeping rows.
type stepMarkerDetail struct {
	Error    string `json:"error,omitempty"`
	BudgetMS int64  `json:"budget_ms"`
}

// journalStepMarker records a runner bookkeeping row (step_partial /
// step_error) for step name. Best-effort: a journal failure is logged,
// not fatal, because the marker is observability and must not mask the
// step outcome already captured in PassResult.
func journalStepMarker(ctx context.Context, pc *PassContext, name string, kind OpKind, stepErr error, budget time.Duration, logger *slog.Logger) {
	detail := stepMarkerDetail{BudgetMS: budget.Milliseconds()}
	if stepErr != nil {
		detail.Error = stepErr.Error()
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		// Marshalling a two-field struct of scalars cannot fail in
		// practice; guard anyway so a future field change degrades to
		// an empty detail instead of a dropped marker.
		raw = nil
	}
	if _, err := RecordOp(ctx, pc.LoreDB, Op{
		PassID:  pc.PassID,
		Step:    name,
		Policy:  PolicyAuto,
		Kind:    kind,
		Target:  runnerOpTarget,
		Detail:  string(raw),
		Applied: false,
	}); err != nil {
		logger.Warn("sleep journal marker failed",
			slog.Int64("pass_id", pc.PassID),
			slog.String("step", name),
			slog.String("kind", string(kind)),
			slog.String("err", err.Error()),
		)
	}
}
