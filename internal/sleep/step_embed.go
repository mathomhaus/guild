package sleep

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// EmbedStep drives vector coverage toward 100% during sleep passes by
// running embed.Backfill against every registered corpus (lore on
// pc.LoreDB, quest on pc.QuestDB).
//
// Acting condition: any eligible source with a missing, content-stale, or
// incompatible vector. Startup and idle repair use the same coverage API;
// the pass wall budget bounds this step's encoding work. Cancellation is safe
// and partial persisted progress is journaled.
//
// Safety: Backfill checks model identity, canonical source text and eligibility
// under the writer lock before inserting or repairing a row. Racing repair
// passes cannot overwrite a concurrent source edit. Derived vector repair is
// classified PolicyAuto by the HYBRID gate (see policy.go).
type EmbedStep struct{}

// Compile-time check: EmbedStep must satisfy Step.
var _ Step = EmbedStep{}

const (
	// embedStepName identifies this step in journal rows and logs.
	embedStepName = "embed"

	// embedJournalGrace bounds the audit-trail write for an op whose
	// work already ran when the pass wall budget expired. Mirrors the
	// runner's endPassGrace rationale: the journal must record what
	// happened even when the budget context is already cancelled, so
	// the write runs under context.WithoutCancel plus this timeout.
	embedJournalGrace = 5 * time.Second

	// skipReasonEmbedderDisabled is recorded when EmbedDeps is nil or
	// not Enabled(): the machine has no working embedder, so the step
	// is a clean journaled no-op, not an error.
	skipReasonEmbedderDisabled = "embedder_disabled"

	// skipReasonQuestDBNotWired is recorded when the caller did not
	// wire pc.QuestDB; the quest corpus cannot be backfilled this pass.
	skipReasonQuestDBNotWired = "quest_db_not_wired"
)

// embedOpDetail is the detail JSON for one corpus backfill op.
type embedOpDetail struct {
	Corpus string `json:"corpus"`
	// PendingBefore includes missing, stale and incompatible repair candidates.
	PendingBefore int64 `json:"pending_before"`
	// Embedded counts actual persisted insertions and repairs.
	Embedded int `json:"embedded"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	// Cancelled is true when the pass budget (or caller cancellation)
	// stopped the backfill mid-flight; the counts above then describe
	// partial progress.
	Cancelled bool `json:"cancelled,omitempty"`
	// Error carries a non-budget backfill failure, truncated upstream
	// by embed.Backfill's own logging discipline.
	Error string `json:"error,omitempty"`
}

// embedSkipDetail is the detail JSON for a journaled no-op row.
type embedSkipDetail struct {
	Corpus  string `json:"corpus,omitempty"`
	Skipped string `json:"skipped"`
}

// embedOpInverse records how to manually reverse an applied backfill.
// Vectors are derived data. Deleting recently encoded rows schedules another
// repair; it does not restore replaced vector values. The timestamp boundary
// is approximate and can include writes from a concurrent process.
type embedOpInverse struct {
	VectorTable  string `json:"vector_table"`
	EncodedAtGTE int64  `json:"encoded_at_gte"`
	Rows         int    `json:"rows"`
	Note         string `json:"note"`
}

// Name identifies the step in journal rows and logs.
func (EmbedStep) Name() string { return embedStepName }

// Run backfills vectors for every corpus with pending entities. One
// corpus failing does not abort the others (mirrors the per-target
// isolation in the startup auto-backfill); a budget cancellation
// journals partial progress and returns the cancellation error so the
// runner marks the step partial.
func (EmbedStep) Run(ctx context.Context, pc *PassContext) (StepReport, error) {
	if pc == nil || pc.LoreDB == nil {
		return StepReport{}, fmt.Errorf("sleep: embed step: nil pass context or lore db")
	}
	logger := pc.logger()

	// Consult the HYBRID gate for derived-data repair: a future taxonomy change must not leave this
	// step silently mutating against policy.
	if Classify(OpEmbedBackfill) != PolicyAuto {
		return StepReport{}, fmt.Errorf("sleep: embed step: op %s is no longer classified %s; step must not mutate unattended", OpEmbedBackfill, PolicyAuto)
	}

	var report StepReport

	// No working embedder on this machine: clean journaled no-op.
	if !pc.Embed.Enabled() {
		if err := recordEmbedSkip(ctx, pc, runnerOpTarget, "", skipReasonEmbedderDisabled); err != nil {
			return report, err
		}
		report.Note = "skipped: no enabled embedder"
		return report, nil
	}

	// Target registry, mirroring the corpus set the startup
	// auto-backfill iterates (internal/mcp): LoreCorpus on lore.db,
	// QuestCorpus on quest.db. Handles come from PassContext instead of
	// package-level openers so the daemon single-writer discipline
	// holds when the daemon is the caller.
	targets := []struct {
		corpus embed.VectorCorpus
		db     *sql.DB
		// skipReason names why the target is unprocessable when db is
		// nil. Only quest can be nil today; LoreDB is runner-validated.
		skipReason string
	}{
		{corpus: embed.LoreCorpus{}, db: pc.LoreDB},
		{corpus: embed.QuestCorpus{}, db: pc.QuestDB, skipReason: skipReasonQuestDBNotWired},
	}

	var (
		errs  []error
		notes []string
	)
	for _, tgt := range targets {
		name := tgt.corpus.Name()

		if err := ctx.Err(); err != nil {
			// Budget expired (or the caller cancelled) before this
			// corpus started: nothing ran for it, so nothing is
			// journaled. Mirrors the runner's skip semantics.
			report.Note = joinNotes(notes)
			return report, err
		}
		if tgt.db == nil {
			if err := recordEmbedSkip(ctx, pc, name, name, tgt.skipReason); err != nil {
				errs = append(errs, err)
			}
			notes = append(notes, name+": skipped ("+tgt.skipReason+")")
			continue
		}
		if pc.Caps.MaxAutoOps > 0 && report.OpsApplied >= pc.Caps.MaxAutoOps {
			// Per-pass auto-op cap exhausted; defer this corpus to the
			// next pass. The source-derived coverage scan picks it up again.
			notes = append(notes, name+": deferred (auto-op cap reached)")
			continue
		}

		pending, err := countPending(ctx, tgt.db, tgt.corpus, pc.Embed.ModelID)
		if err != nil {
			if ctx.Err() != nil {
				report.Note = joinNotes(notes)
				return report, ctx.Err()
			}
			errs = append(errs, fmt.Errorf("sleep: embed step: %s: count pending: %w", name, err))
			continue
		}
		if pending == 0 {
			// Full coverage already. Quiet on purpose, no journal row:
			// the healthy case is the common path, and a recurring
			// no-op row per pass would drown the journal.
			continue
		}

		// encoded_at lower bound for the inverse payload. Real clock,
		// not pc.now(): insertVectorRow stamps rows with the real
		// clock, so the manual-reversal boundary must match it.
		startUnix := time.Now().UTC().Unix()

		res, backErr := embed.Backfill(ctx, embed.BackfillOptions{
			DB:       tgt.db,
			Corpus:   tgt.corpus,
			Embedder: pc.Embed.Embedder,
			ModelID:  pc.Embed.ModelID,
			Logger:   logger,
		})

		cancelled := backErr != nil &&
			(errors.Is(backErr, context.Canceled) || errors.Is(backErr, context.DeadlineExceeded))

		detail := embedOpDetail{Corpus: name, PendingBefore: pending, Cancelled: cancelled}
		if res != nil {
			detail.Embedded = res.Embedded
			detail.Failed = res.Failed
			detail.Skipped = res.Skipped
		}
		if backErr != nil && !cancelled {
			detail.Error = backErr.Error()
		}

		applied := res != nil && res.Embedded > 0
		op := Op{
			PassID:  pc.PassID,
			Step:    embedStepName,
			Policy:  PolicyAuto,
			Kind:    OpEmbedBackfill,
			Target:  name,
			Detail:  marshalJSON(detail),
			Applied: applied,
		}
		if applied {
			op.Inverse = marshalJSON(embedOpInverse{
				VectorTable:  tgt.corpus.VectorTable(),
				EncodedAtGTE: startUnix,
				Rows:         res.Embedded,
				Note:         "deleting recently encoded rows schedules recomputation; replaced values are not restored, and the timestamp boundary may include concurrent writes",
			})
		}
		if err := recordEmbedOp(ctx, pc, op); err != nil {
			errs = append(errs, err)
		}
		if applied {
			report.OpsApplied++
		}
		if res != nil {
			notes = append(notes, fmt.Sprintf("%s: %d inserted or repaired, %d skipped, %d failed", name, res.Embedded, res.Skipped, res.Failed))
		}

		if cancelled {
			// Partial progress is journaled above; surface the
			// cancellation so the runner marks the step partial.
			report.Note = joinNotes(notes)
			return report, backErr
		}
		if backErr != nil {
			// Non-budget failure on this corpus must not abort the
			// others.
			errs = append(errs, fmt.Errorf("sleep: embed step: %s: %w", name, backErr))
		}
	}

	report.Note = joinNotes(notes)
	return report, errors.Join(errs...)
}

// countPending measures repair candidates from current eligible source rows,
// including existing vectors whose model, shape or content hash is stale.
func countPending(ctx context.Context, db *sql.DB, corpus embed.VectorCorpus, modelID string) (int64, error) {
	coverage, err := embed.ReadCoverage(ctx, db, corpus, modelID, nil)
	if err != nil {
		return 0, err
	}
	return coverage.Missing + coverage.Stale + coverage.Invalid, nil
}

// recordEmbedOp journals op through a cancellation-shielded context:
// the mutation it describes already happened, so the audit row must
// land even when the pass wall budget expired mid-backfill (the same
// WithoutCancel rationale as the runner's EndPass write). A journal
// failure surfaces as a step error because the journal is what keeps
// the HYBRID auto-apply policy honest.
func recordEmbedOp(ctx context.Context, pc *PassContext, op Op) error {
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), embedJournalGrace)
	defer cancel()
	if _, err := RecordOp(jctx, pc.LoreDB, op); err != nil {
		return fmt.Errorf("sleep: embed step: journal %s op: %w", op.Target, err)
	}
	return nil
}

// recordEmbedSkip journals an unapplied no-op row naming why a target
// (or the whole step) did not act.
func recordEmbedSkip(ctx context.Context, pc *PassContext, target, corpus, reason string) error {
	return recordEmbedOp(ctx, pc, Op{
		PassID:  pc.PassID,
		Step:    embedStepName,
		Policy:  PolicyAuto,
		Kind:    OpEmbedBackfill,
		Target:  target,
		Detail:  marshalJSON(embedSkipDetail{Corpus: corpus, Skipped: reason}),
		Applied: false,
	})
}

// marshalJSON renders v for a journal payload column. Marshalling the
// small scalar-only structs in this file cannot fail in practice; the
// empty-string fallback degrades to a NULL column instead of dropping
// the row.
func marshalJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// joinNotes renders the per-corpus outcome notes as one narration line.
func joinNotes(notes []string) string {
	if len(notes) == 0 {
		return "no pending entities"
	}
	return strings.Join(notes, "; ")
}
