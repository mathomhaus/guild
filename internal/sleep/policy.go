// Package sleep is the substrate for autonomous maintenance ("sleep")
// passes: a durable journal (journal.go), a mutation policy gate
// (policy.go), and a bounded step runner (runner.go).
//
// The package is deliberately transport-neutral. It must not import
// internal/daemon or internal/mcp: the same steps run inside the
// daemon's idle scheduler and inside the degraded in-process autopass,
// mirroring how internal/command keeps handlers transport-agnostic.
// Step implementations (consolidation, echo renewal, embed backfill)
// live in sibling packages and plug in via the Step interface.
package sleep

// Policy is the mutation-gate verdict for one op kind: apply it
// unattended (with full journaling) or post an approval quest instead
// of mutating.
type Policy string

const (
	// PolicyAuto means the op is additive or trivially reversible, so a
	// sleep pass applies it unattended and journals an inverse payload.
	PolicyAuto Policy = "auto"

	// PolicyApproval means the op is destructive or judgment-laden, so a
	// sleep pass posts an approval quest describing the proposed change
	// instead of mutating.
	PolicyApproval Policy = "approval"
)

// OpKind identifies one category of sleep-pass mutation. The constants
// below are the complete defined taxonomy; Classify treats anything
// else as unknown and default-denies it to PolicyApproval.
type OpKind string

const (
	// OpMeldExactSupersede supersedes the older half of an exact
	// duplicate pair (lore.MeldPair with Score == 1.0, the normalized
	// hash-collision pass in internal/lore/meld.go). Precedent for
	// applying this unattended: lore.Commune's fix path feeds itself
	// exact-only pairs via Meld(ctx, db, 1.0, true, "") and
	// auto-supersedes them without a human in the loop. The status flip
	// plus supersedes edge are recorded as the inverse payload, so the
	// op is manually reversible.
	OpMeldExactSupersede OpKind = "meld_exact_supersede"

	// OpEmbedBackfill encodes pending corpus rows into vector tables.
	// Purely additive: every vector write is INSERT OR IGNORE (ADR-003
	// invariant 1; see the cross-process safety note in
	// internal/mcp/embed_autobackfill.go), so racing writers and re-runs
	// converge on the same rows.
	OpEmbedBackfill OpKind = "embed_backfill"

	// OpRenewalQuestPost posts a quest asking for a fading echo to be
	// renewed or retired. Posting a quest is additive by construction:
	// it creates a new row and touches nothing existing.
	OpRenewalQuestPost OpKind = "renewal_quest_post"

	// OpApprovalQuestPost posts the approval quest that a
	// PolicyApproval op produces instead of mutating. The post itself
	// is additive by construction, even though the change it proposes
	// is not.
	OpApprovalQuestPost OpKind = "approval_quest_post"

	// OpNearMeld merges a near-duplicate pair (lore.MeldPair with
	// Score < 1.0, the Jaccard pass). Below the exact threshold the
	// pair may be two genuinely distinct entries; collapsing them
	// destroys information, so a human (or interactive agent) decides.
	OpNearMeld OpKind = "near_meld"

	// OpReforge supersedes one entry with another (lore.Reforge):
	// it rewrites which entry is canonical. Outside the exact-duplicate
	// case that judgment is not mechanical.
	OpReforge OpKind = "reforge"

	// OpSeal archives an entry (lore.Seal), removing it from coverage
	// and from default reads. Hiding knowledge is destructive enough to
	// warrant approval.
	OpSeal OpKind = "seal"

	// OpOathDemotion reclassifies a bloated principle to kind=decision
	// (the lore.Commune fix-path reclassify). It changes how an entry
	// is loaded at every future session start, so it stays gated.
	OpOathDemotion OpKind = "oath_demotion"
)

// Classify returns the HYBRID-policy verdict for op: additive or
// reversible ops auto-apply with full journaling; destructive ops post
// approval quests instead of mutating. Unknown op kinds default to
// PolicyApproval (default-deny), so a step that invents a new op kind
// without extending this gate cannot accidentally mutate unattended.
func Classify(op OpKind) Policy {
	switch op {
	case OpMeldExactSupersede, OpEmbedBackfill, OpRenewalQuestPost, OpApprovalQuestPost:
		return PolicyAuto
	case OpNearMeld, OpReforge, OpSeal, OpOathDemotion:
		return PolicyApproval
	default:
		return PolicyApproval
	}
}
