package sleep

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Narration: the read side of the sleep journal. A session start
// consumes every completed pass no session has narrated yet and folds
// the journaled ops into one "while you slept" line, so unattended
// maintenance is surfaced to the operator exactly once. The
// consumed-flag lives on the pass row (narrated_at), claimed through
// MarkNarrated's guarded UPDATE, so two racing sessions split the
// passes between them and no pass is announced twice.

// narrationPrefix opens the line. Moon-prefixed so collapsed-view
// clients show at a glance that unattended work happened.
const narrationPrefix = "🌙 while you slept"

// opCountDetail is the optional JSON shape batch ops use in
// Op.Detail. A step that journals one row covering N entries (the
// embed-backfill batch case) records {"count": N}; absent or invalid
// detail counts as one entry. This is the read-side half of the
// contract step implementations write to.
type opCountDetail struct {
	Count int `json:"count"`
}

// opCount returns how many entries op covers: Detail's "count" field
// when present and positive, else 1.
func opCount(op Op) int {
	if op.Detail != "" {
		var d opCountDetail
		if err := json.Unmarshal([]byte(op.Detail), &d); err == nil && d.Count > 0 {
			return d.Count
		}
	}
	return 1
}

// Summarize folds journal rows into the single "while you slept"
// narration line, aggregating across every pass in passes (the
// operator may have been away for several). Counted segments, each
// omitted when zero, singular/plural handled:
//
//   - melds applied (auto-applied exact-duplicate supersedes)
//   - renewal bounties posted
//   - entries embedded (batch rows weighted by Detail count)
//   - ops awaiting review (every row with the approval policy verdict:
//     the gated proposals the pass posted approval quests for)
//   - steps deferred by budget (runner step_partial markers)
//
// Approval rows are counted once via the policy verdict, not again via
// the approval_quest_post rows that carry the posts themselves, so a
// gated op never double-counts. Entry-level detail stays in the
// journal and the approval quests; the line is a hard one-liner.
//
// Returns "" when passes is empty: nothing completed, nothing to say.
func Summarize(passes []Pass, ops []Op) string {
	if len(passes) == 0 {
		return ""
	}

	var melds, renewals, embedded, awaiting, deferred int
	for _, op := range ops {
		if op.Policy == PolicyApproval {
			// A gated proposal: the pass posted an approval quest
			// instead of mutating. Counted here regardless of kind so
			// the operator learns review is wanted.
			awaiting++
			continue
		}
		switch op.Kind {
		case OpMeldExactSupersede:
			if op.Applied {
				melds++
			}
		case OpRenewalQuestPost:
			if op.Applied {
				renewals++
			}
		case OpEmbedBackfill:
			if op.Applied {
				embedded += opCount(op)
			}
		case opStepPartial:
			deferred++
		}
	}

	var parts []string
	if melds > 0 {
		parts = append(parts, fmt.Sprintf("melded %d %s", melds, plural(melds, "entry", "entries")))
	}
	if renewals > 0 {
		parts = append(parts, fmt.Sprintf("posted %d renewal %s", renewals, plural(renewals, "bounty", "bounties")))
	}
	if embedded > 0 {
		parts = append(parts, fmt.Sprintf("embedded %d %s", embedded, plural(embedded, "entry", "entries")))
	}
	if awaiting > 0 {
		parts = append(parts, fmt.Sprintf("%d %s awaiting review", awaiting, plural(awaiting, "op", "ops")))
	}
	if deferred > 0 {
		parts = append(parts, fmt.Sprintf("%d %s deferred by budget", deferred, plural(deferred, "step", "steps")))
	}

	head := narrationPrefix
	if len(passes) > 1 {
		head += fmt.Sprintf(" (%d passes)", len(passes))
	}
	if len(parts) == 0 {
		// Passes ran but applied nothing (e.g. an up-to-date corpus).
		// Still narrate: the consumed flag was spent on them, and the
		// operator should know the pass happened.
		return head + ": no changes"
	}
	return head + ": " + strings.Join(parts, ", ")
}

// Narrate claims every completed, unnarrated pass and returns the
// one-line summary of what those passes did. Returns "" when there is
// nothing to narrate (no completed passes, or another session claimed
// them first).
//
// Exactly-once: each pass is claimed through MarkNarrated's guarded
// UPDATE before its ops are read, so concurrent callers split the
// unnarrated passes between them and every pass lands in exactly one
// caller's line. In-flight passes (no ended_at) are not returned by
// UnnarratedPasses and stay for a later session.
//
// If the journal fails mid-loop after at least one pass was claimed,
// Narrate stops and summarizes what it already owns rather than
// dropping claimed passes silently: the consumed flag is already
// spent on them, and the full audit trail remains in the journal
// regardless. A zero now defaults to time.Now().UTC().
func Narrate(ctx context.Context, db *sql.DB, now time.Time) (string, error) {
	if db == nil {
		return "", fmt.Errorf("sleep: narrate: nil db")
	}

	passes, err := UnnarratedPasses(ctx, db)
	if err != nil {
		return "", fmt.Errorf("sleep: narrate: %w", err)
	}

	var claimed []Pass
	var ops []Op
	for _, p := range passes {
		mine, err := MarkNarrated(ctx, db, p.ID, now)
		if err != nil {
			if len(claimed) > 0 {
				break
			}
			return "", fmt.Errorf("sleep: narrate: %w", err)
		}
		if !mine {
			// A concurrent session claimed this pass between our read
			// and our mark; it narrates the pass, we skip it.
			continue
		}
		claimed = append(claimed, p)
		passOps, err := PassOps(ctx, db, p.ID)
		if err != nil {
			// The pass is already consumed; narrate it undercounted
			// rather than lose it. Detail survives in the journal.
			break
		}
		ops = append(ops, passOps...)
	}
	return Summarize(claimed, ops), nil
}

// plural picks the singular or plural noun for n.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return singular
	}
	return pluralForm
}
