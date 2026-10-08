package sleep

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
)

// The renewal step is the lore-to-quest decay path: knowledge that has
// gone stale generates work instead of silently rotting on the board.
// Today fading echoes render as a passive list at session start and
// nothing happens; this step upgrades the signal to capped, deduplicated
// renewal bounties a future session can pick up.
//
// Detection is NOT reimplemented here. The stale set comes from
// lore.Echoes (internal/lore/echoes.go), which returns only
// current-status entries whose per-kind valid_days window has lapsed or
// (git-aware) whose source file changed in git after the entry was
// inscribed, each paired with a human-readable reason string. Posting is
// NOT reimplemented either: each pass hands its candidates to
// quest.PostRenewals (internal/quest/renewal.go), the shared poster that
// owns deterministic oldest-first selection, dedupe against open renewal
// quests, and the capped post. This step is the sleep-pass caller that
// poster was designed for.
//
// HYBRID policy: a renewal post is additive by construction (it creates
// a new quest row and touches nothing existing), so Classify gates
// OpRenewalQuestPost to PolicyAuto. The step itself never mutates lore;
// the destructive judgment (re-validate vs. supersede vs. retire) is
// exactly what the posted quest routes to a human or interactive agent.
// That is why even the additive action here is itself a quest.

// StepNameRenewal is the Step.Name() of the renewal step, the label its
// journal rows carry in sleep_ops.
const StepNameRenewal = "renewal"

// renewalJournalGrace bounds an audit-trail write whose work already
// landed when the pass wall budget expired. Mirrors the runner's
// endPassGrace rationale: the journal must record what happened even
// when the budget context is already cancelled, so the write runs under
// context.WithoutCancel plus this timeout.
const renewalJournalGrace = 5 * time.Second

// Journaled no-op reasons. A clean skip is recorded (not silently
// dropped) so narration can explain why a pass posted nothing.
const (
	// reasonRenewalQuestDBNotWired is recorded when the caller did not
	// wire pc.QuestDB; renewal quests have nowhere to land this pass.
	reasonRenewalQuestDBNotWired = "quest_db_not_wired"

	// reasonRenewalCapZero is recorded when the per-pass renewal cap is
	// zero: the step is configured to post nothing this pass.
	reasonRenewalCapZero = "renewal_cap_zero"
)

// RenewalStep implements Step. The zero value is usable; the cap comes
// from pc.Caps.MaxRenewalPosts so the scheduler / autopass caller
// threads config through without per-step state.
type RenewalStep struct{}

// Compile-time check: RenewalStep must satisfy Step.
var _ Step = RenewalStep{}

// renewalOpDetail is the detail JSON for one posted renewal quest.
type renewalOpDetail struct {
	Project string `json:"project"`
	Entry   string `json:"entry"`
	Quest   string `json:"quest"`
	Reason  string `json:"reason"`
}

// renewalSkipDetail is the detail JSON for a journaled no-op row: the
// step (or a project) posted nothing, with the reason why.
type renewalSkipDetail struct {
	Project string `json:"project,omitempty"`
	Skipped string `json:"skipped"`
}

// renewalOverflowDetail is the detail JSON for the per-pass overflow
// marker: how many stale entries the cap left waiting for a later pass.
// Narration reads Suppressed to say "and N more waiting".
type renewalOverflowDetail struct {
	Suppressed int `json:"suppressed"`
}

// Name implements Step.
func (RenewalStep) Name() string { return StepNameRenewal }

// Run scans every registered project for fading echoes and posts capped,
// deduplicated renewal quests for the oldest entries first.
//
// The work is bounded three ways: lore.Echoes only flags current entries
// past their validity window (or git-changed), pc.Caps.MaxRenewalPosts
// caps posts per pass, and quest.PostRenewals dedupes against any open
// renewal quest for the same entry. Posting is additive, so the whole
// step runs under PolicyAuto with every post journaled in sleep_ops.
//
// Cancellation: the step checks ctx before each project and only posts
// while budget remains. quest.PostRenewals threads ctx into each post,
// so a mid-batch cancellation stops cleanly with the already-committed
// posts journaled.
func (RenewalStep) Run(ctx context.Context, pc *PassContext) (StepReport, error) {
	if pc == nil || pc.LoreDB == nil {
		return StepReport{}, fmt.Errorf("sleep: renewal step: nil pass context or lore db")
	}
	logger := pc.logger()

	// Consult the HYBRID gate even though OpRenewalQuestPost is additive
	// by construction: a future taxonomy change must not leave this step
	// silently acting against policy.
	if Classify(OpRenewalQuestPost) != PolicyAuto {
		return StepReport{}, fmt.Errorf("sleep: renewal step: op %s is no longer classified %s; step must not act unattended", OpRenewalQuestPost, PolicyAuto)
	}

	var report StepReport

	// No quest db wired: renewal quests have nowhere to land. Clean
	// journaled no-op, same contract as the embed step's nil-corpus skip.
	if pc.QuestDB == nil {
		if err := recordRenewalSkip(ctx, pc, "", reasonRenewalQuestDBNotWired); err != nil {
			return report, err
		}
		report.Note = "skipped: no quest db wired"
		return report, nil
	}

	// The per-pass renewal cap is the upper bound on posts across every
	// project this pass. A zero (or negative) cap means "post nothing";
	// record it so a misconfigured pass is visible in the journal rather
	// than silently inert.
	maxPosts := pc.Caps.MaxRenewalPosts
	if maxPosts <= 0 {
		if err := recordRenewalSkip(ctx, pc, "", reasonRenewalCapZero); err != nil {
			return report, err
		}
		report.Note = "skipped: renewal cap is zero"
		return report, nil
	}

	projects, err := project.List(ctx, pc.LoreDB)
	if err != nil {
		return report, fmt.Errorf("sleep: renewal step: list projects: %w", err)
	}

	var (
		suppressed int
		notes      []string
	)
	for i := range projects {
		if err := ctx.Err(); err != nil {
			// Budget expired (or the caller cancelled) before this project
			// started. Posts already committed are journaled; surface the
			// cancellation so the runner marks the step partial.
			report.Note = joinNotes(notes)
			return report, err
		}

		pid := projects[i].ID

		// The cap is global across the pass: once spent, every remaining
		// project's stale entries are overflow. Stop posting and account
		// for what is left so narration stays honest.
		remaining := maxPosts - report.OpsPosted
		if remaining <= 0 {
			overflow, err := countStale(ctx, pc, pid)
			if err != nil {
				return report, err
			}
			suppressed += overflow
			continue
		}

		echoes, err := lore.Echoes(ctx, pc.LoreDB, pid, true)
		if err != nil {
			if ctx.Err() != nil {
				report.Note = joinNotes(notes)
				return report, ctx.Err()
			}
			return report, fmt.Errorf("sleep: renewal step: echoes for %s: %w", pid, err)
		}
		if len(echoes) == 0 {
			// Healthy project: no fading echoes. Quiet on purpose, no
			// journal row, since a recurring empty row per project per pass
			// would drown the journal (same rationale as the embed step's
			// zero-pending skip).
			continue
		}

		candidates := toRenewalCandidates(echoes)

		// PostRenewals owns selection, dedupe, and the cap; hand it the
		// remaining budget so the pass-wide cap holds even across multiple
		// projects. It returns Posted / Deduped / OverCap so the step can
		// journal each post and roll overflow into the suppressed count.
		res, postErr := quest.PostRenewals(ctx, pc.QuestDB, pid, candidates, remaining, approvalAgent)
		if res != nil {
			for j := range res.Posted {
				p := res.Posted[j]
				op := Op{
					PassID: pc.PassID,
					Step:   StepNameRenewal,
					Policy: PolicyAuto,
					Kind:   OpRenewalQuestPost,
					Target: p.QuestID,
					Detail: marshalJSON(renewalOpDetail{
						Project: pid,
						Entry:   lore.EntryID(p.EntryID),
						Quest:   p.QuestID,
						Reason:  reasonFor(echoes, p.EntryID),
					}),
					Applied: true,
				}
				if jErr := recordRenewalOp(ctx, pc, op); jErr != nil {
					return report, jErr
				}
				report.OpsPosted++
			}
			suppressed += len(res.OverCap)
		}
		if postErr != nil {
			if ctx.Err() != nil {
				report.Note = joinNotes(notes)
				return report, ctx.Err()
			}
			return report, fmt.Errorf("sleep: renewal step: post renewals for %s: %w", pid, postErr)
		}

		if res != nil && len(res.Posted) > 0 {
			notes = append(notes, fmt.Sprintf("%s: %d posted", pid, len(res.Posted)))
		}
	}

	// One overflow marker per pass when the cap left work waiting, so
	// narration can say "and N more waiting" without re-scanning.
	if suppressed > 0 {
		if err := recordRenewalOverflow(ctx, pc, suppressed); err != nil {
			return report, err
		}
		notes = append(notes, fmt.Sprintf("%d over cap", suppressed))
	}

	logger.Info("sleep renewal step complete",
		slog.Int64("pass_id", pc.PassID),
		slog.Int("posted", report.OpsPosted),
		slog.Int("suppressed", suppressed),
	)

	report.Note = joinNotes(notes)
	return report, nil
}

// toRenewalCandidates maps lore echoes to the poster's neutral candidate
// descriptor. The poster re-sorts oldest-first, so input order does not
// matter, but Echoes already returns created_at ASC.
func toRenewalCandidates(echoes []lore.Echo) []quest.RenewalCandidate {
	out := make([]quest.RenewalCandidate, 0, len(echoes))
	for i := range echoes {
		e := echoes[i].Entry
		if e == nil {
			continue
		}
		out = append(out, quest.RenewalCandidate{
			EntryID:  e.ID,
			Title:    e.Title,
			Kind:     string(e.Kind),
			FilePath: e.FilePath,
			Reason:   echoes[i].Reason,
		})
	}
	return out
}

// reasonFor returns the staleness reason Echoes attached to entryID, or
// "" when the id is not present (cannot happen for posted ids, which all
// came from these echoes).
func reasonFor(echoes []lore.Echo, entryID int64) string {
	for i := range echoes {
		if echoes[i].Entry != nil && echoes[i].Entry.ID == entryID {
			return echoes[i].Reason
		}
	}
	return ""
}

// countStale counts the fading echoes in one project without posting,
// to account for overflow in projects the spent cap never reached.
func countStale(ctx context.Context, pc *PassContext, projectID string) (int, error) {
	echoes, err := lore.Echoes(ctx, pc.LoreDB, projectID, true)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("sleep: renewal step: count stale for %s: %w", projectID, err)
	}
	return len(echoes), nil
}

// recordRenewalOp journals one op through a cancellation-shielded
// context: the quest it describes is already committed, so the audit row
// must land even when the pass wall budget expired mid-pass (the same
// WithoutCancel rationale as the runner's EndPass write). A journal
// failure surfaces as a step error because the journal is what keeps the
// HYBRID auto-apply policy honest.
func recordRenewalOp(ctx context.Context, pc *PassContext, op Op) error {
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renewalJournalGrace)
	defer cancel()
	if _, err := RecordOp(jctx, pc.LoreDB, op); err != nil {
		return fmt.Errorf("sleep: renewal step: journal %s op: %w", op.Target, err)
	}
	return nil
}

// recordRenewalSkip journals an unapplied no-op row naming why the step
// (or a project) posted nothing.
func recordRenewalSkip(ctx context.Context, pc *PassContext, projectID, reason string) error {
	return recordRenewalOp(ctx, pc, Op{
		PassID:  pc.PassID,
		Step:    StepNameRenewal,
		Policy:  PolicyAuto,
		Kind:    OpRenewalQuestPost,
		Target:  runnerOpTarget,
		Detail:  marshalJSON(renewalSkipDetail{Project: projectID, Skipped: reason}),
		Applied: false,
	})
}

// recordRenewalOverflow journals the one per-pass overflow marker: the
// count of stale entries the cap left for a later pass. Applied is false
// because nothing was posted for these entries; the row is observability
// for narration.
func recordRenewalOverflow(ctx context.Context, pc *PassContext, suppressed int) error {
	return recordRenewalOp(ctx, pc, Op{
		PassID:  pc.PassID,
		Step:    StepNameRenewal,
		Policy:  PolicyAuto,
		Kind:    OpRenewalQuestPost,
		Target:  runnerOpTarget,
		Detail:  marshalJSON(renewalOverflowDetail{Suppressed: suppressed}),
		Applied: false,
	})
}
