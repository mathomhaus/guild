package sleep

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
)

// The consolidation step is the heart of the sleep cycle: it turns idle
// time into a smaller, deduplicated lore corpus while honoring the
// HYBRID mutation policy (policy.go). The line between auto and
// approval is information loss, not verb:
//
//   - Exact normalized duplicates (the hash-collision pass in
//     lore.Meld via lore.Commune) lose zero information when the older
//     copy is superseded: the superseded row keeps its full content and
//     the journaled inverse records the prior status plus the
//     supersedes edge. Classify(OpMeldExactSupersede) == PolicyAuto.
//   - Near-duplicates, kind-drift pairs, and oath demotions all require
//     judgment, so the step posts one approval quest per candidate
//     instead of mutating. Classify gates them to PolicyApproval.
//
// Detection is NOT reimplemented here: exact pairs come from
// lore.Commune (fix=false), which feeds itself exact-only pairs via
// lore.Meld(ctx, db, 1.0, true, ""); near pairs come from one
// lore.Meld call at the configured threshold; oath-bloat candidates
// come from the same Commune report. lore.Seal is never called from
// this step, and Commune is never invoked with fix=true (a source-level
// test in step_consolidate_test.go locks both).

// StepNameConsolidate is the Step.Name() of the consolidation step, the
// label its journal rows carry in sleep_ops.
const StepNameConsolidate = "consolidate"

// Default knobs for ConsolidateStep zero values. The caller that
// assembles the pass (daemon idle scheduler / in-process autopass) is
// expected to thread config through; these mirror the config defaults
// so a zero-value step behaves like a default install.
const (
	// DefaultNearMeldThreshold is the Jaccard floor for near-duplicate
	// detection, matching the `lore meld --threshold` default.
	DefaultNearMeldThreshold = 0.9

	// DefaultBloatBoundary mirrors cfg.Inscribe.PrincipleMaxWords.
	DefaultBloatBoundary = lore.PrincipleMaxWordsDefault

	// DefaultSevereBoundary mirrors cfg.Inscribe.BloatSevereThreshold:
	// principles above it are what `lore commune --fix` would demote.
	DefaultSevereBoundary = 120
)

// ApprovalEpic is the campaign every sleep-posted approval quest joins,
// so the board groups them and a human can sweep the queue in one view.
const ApprovalEpic = "sleep-approvals"

// approvalPriority is the priority of sleep-posted approval quests:
// maintenance, never urgent.
const approvalPriority = quest.Priority("P3")

// approvalAgent is the [spec]-note author for sleep-posted quests, so
// scroll output attributes them to the sleep cycle rather than to a
// generic "agent".
const approvalAgent = "sleep"

// Deferral reasons recorded in op detail payloads when a candidate is
// journaled instead of acted on. Deferred work is picked up by the next
// pass; journaling it keeps the cap from silently dropping candidates.
const (
	reasonAutoOpCap    = "auto op cap reached"
	reasonQuestPostCap = "quest post cap reached"
	reasonNoQuestDB    = "quest db unavailable"
)

// ConsolidateStep implements Step. The zero value is usable and runs
// with the documented defaults; callers wire config-backed values in.
type ConsolidateStep struct {
	// NearThreshold is the Jaccard similarity floor for near-duplicate
	// detection. <=0 defaults to DefaultNearMeldThreshold; values >1
	// clamp to 1 (exact-only detection, no near-match scan).
	NearThreshold float64

	// BloatBoundary is the word count above which a principle counts as
	// bloat (cfg.Inscribe.PrincipleMaxWords). <=0 defaults to
	// DefaultBloatBoundary.
	BloatBoundary int

	// SevereBoundary is the word count above which a bloated principle
	// becomes a demotion candidate (cfg.Inscribe.BloatSevereThreshold).
	// <=0 defaults to DefaultSevereBoundary.
	SevereBoundary int
}

// Name implements Step.
func (s *ConsolidateStep) Name() string { return StepNameConsolidate }

// meldDetail is the JSON detail payload for meld ops (auto and gated).
type meldDetail struct {
	Left         string  `json:"left"`
	Right        string  `json:"right"`
	LeftProject  string  `json:"left_project"`
	RightProject string  `json:"right_project"`
	Score        float64 `json:"score"`
	KindDrift    bool    `json:"kind_drift,omitempty"`
	// Quest / Project are set on approval rows that actually posted.
	Quest   string `json:"quest,omitempty"`
	Project string `json:"project,omitempty"`
	// Deferred / Reason are set when the candidate was journaled
	// instead of acted on (cap reached, quest db unavailable).
	Deferred bool   `json:"deferred,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// meldInverse is the JSON inverse payload for auto-applied exact melds:
// everything needed to manually reverse the op (restore the prior
// status, drop the supersedes edge).
type meldInverse struct {
	PriorStatus string `json:"prior_status"`
	EdgeFrom    int64  `json:"edge_from"`
	EdgeTo      int64  `json:"edge_to"`
	Relation    string `json:"relation"`
}

// demotionDetail is the JSON detail payload for oath-demotion ops.
type demotionDetail struct {
	Entry          string `json:"entry"`
	Project        string `json:"project"`
	WordCount      int    `json:"word_count"`
	SevereBoundary int    `json:"severe_boundary"`
	Quest          string `json:"quest,omitempty"`
	Deferred       bool   `json:"deferred,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// Run implements Step. See the package comment at the top of this file
// for the auto/approval split.
func (s *ConsolidateStep) Run(ctx context.Context, pc *PassContext) (StepReport, error) {
	if pc == nil || pc.LoreDB == nil {
		return StepReport{}, fmt.Errorf("sleep: consolidate: nil lore db")
	}

	nearThreshold := s.NearThreshold
	if nearThreshold <= 0 {
		nearThreshold = DefaultNearMeldThreshold
	}
	if nearThreshold > 1 {
		nearThreshold = 1
	}
	bloatBoundary := s.BloatBoundary
	if bloatBoundary <= 0 {
		bloatBoundary = DefaultBloatBoundary
	}
	severeBoundary := s.SevereBoundary
	if severeBoundary <= 0 {
		severeBoundary = DefaultSevereBoundary
	}

	// Detection, reused not reimplemented. Commune (fix=false: sleep
	// NEVER auto-demotes) supplies both the trusted exact-duplicate set
	// (its internal Meld call runs in exact-only mode, so every pair is
	// a normalized hash collision) and the oath-bloat candidates.
	rep, err := lore.Commune(ctx, pc.LoreDB, "", true, false, bloatBoundary, severeBoundary)
	if err != nil {
		return StepReport{}, fmt.Errorf("sleep: consolidate: %w", err)
	}
	exactKeys := make(map[[2]int64]bool, len(rep.DupPairs))
	for _, p := range rep.DupPairs {
		exactKeys[[2]int64{p.LeftID, p.RightID}] = true
	}

	// One Meld call at the configured threshold yields the full
	// candidate list (exact pairs plus Jaccard near-matches). When the
	// threshold is 1.0 the near scan is skipped and the exact set from
	// Commune is the whole list, so the second call is unnecessary.
	candidates := rep.DupPairs
	if nearThreshold < 1 {
		candidates, err = lore.Meld(ctx, pc.LoreDB, nearThreshold, true, "")
		if err != nil {
			return StepReport{}, fmt.Errorf("sleep: consolidate: %w", err)
		}
	}

	// Partition through the policy gate. Membership in the exact-only
	// set is the auto criterion, NOT Score == 1.0: a Jaccard score can
	// round to 1.0 for reordered-token text that is not a normalized
	// duplicate, and collapsing that pair would lose information.
	// Kind-drift pairs need judgment even when the text is identical.
	var autoPairs, gatedPairs []lore.MeldPair
	for _, p := range candidates {
		kind := OpNearMeld
		if exactKeys[[2]int64{p.LeftID, p.RightID}] && !p.KindDrift {
			kind = OpMeldExactSupersede
		}
		if Classify(kind) == PolicyAuto {
			autoPairs = append(autoPairs, p)
		} else {
			gatedPairs = append(gatedPairs, p)
		}
	}

	// Oath-demotion candidates: the subset commune fix=true would
	// reclassify (WordCount strictly over the severe boundary).
	var demotions []lore.InquestRow
	for _, e := range rep.BloatEntries {
		if e.WordCount > severeBoundary {
			demotions = append(demotions, e)
		}
	}

	// Deterministic ordering so reruns, caps, and tests are stable.
	sortPairs(autoPairs)
	sortPairs(gatedPairs)
	sort.Slice(demotions, func(i, j int) bool { return demotions[i].EntryID < demotions[j].EntryID })

	var report StepReport
	var deferred, suppressed int

	// --- Auto path: supersede older-by-newer via lore.Reforge. ---
	if err := s.applyExactMelds(ctx, pc, autoPairs, &report, &deferred); err != nil {
		return StepReport{}, err
	}

	// --- Approval path: post quests, never mutate. ---
	if len(gatedPairs) > 0 || len(demotions) > 0 {
		var openSubjects []string
		if pc.QuestDB != nil {
			openSubjects, err = loadOpenQuestSubjects(ctx, pc.QuestDB)
			if err != nil {
				return StepReport{}, fmt.Errorf("sleep: consolidate: %w", err)
			}
		}
		if err := s.postMeldApprovals(ctx, pc, gatedPairs, openSubjects, &report, &deferred, &suppressed); err != nil {
			return StepReport{}, err
		}
		if err := s.postDemotionApprovals(ctx, pc, demotions, severeBoundary, openSubjects, &report, &deferred, &suppressed); err != nil {
			return StepReport{}, err
		}
	}

	report.Note = fmt.Sprintf("%d exact melds applied, %d approval quests posted, %d deferred, %d already pending",
		report.OpsApplied, report.OpsPosted, deferred, suppressed)
	return report, nil
}

// applyExactMelds supersedes the older half of each exact-duplicate
// pair with the newer via lore.Reforge (one transaction, the canonical
// supersedes edge, Tx2 re-embed of the survivor), journaling each op
// with an inverse payload. Pairs over Caps.MaxAutoOps are journaled as
// deferred. Chains (A=B=C) collapse naturally: a pair whose older side
// was already superseded this pass is skipped, and processing in
// ascending id order walks the chain toward the newest copy.
func (s *ConsolidateStep) applyExactMelds(ctx context.Context, pc *PassContext, pairs []lore.MeldPair, report *StepReport, deferred *int) error {
	supersededThisPass := make(map[int64]bool)
	for _, p := range pairs {
		// Scheduler preemption (agent activity) aborts cleanly between
		// pairs; everything already applied is journaled.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sleep: consolidate: cancelled: %w", err)
		}
		if supersededThisPass[p.LeftID] {
			continue
		}

		detail := meldDetail{
			Left:         loreRef(p.LeftID),
			Right:        loreRef(p.RightID),
			LeftProject:  p.LeftProject,
			RightProject: p.RightProject,
			Score:        p.Score,
			KindDrift:    p.KindDrift,
		}
		target := fmt.Sprintf("%s<-%s", loreRef(p.RightID), loreRef(p.LeftID))

		if pc.Caps.MaxAutoOps > 0 && report.OpsApplied >= pc.Caps.MaxAutoOps {
			detail.Deferred = true
			detail.Reason = reasonAutoOpCap
			if err := recordConsolidateOp(ctx, pc, OpMeldExactSupersede, target, detail, "", false); err != nil {
				return err
			}
			*deferred++
			continue
		}

		// Capture the prior status for the inverse payload before the
		// mutation. Meld only surfaces status=current entries, but
		// recording the observed value keeps the inverse honest.
		var priorStatus string
		if err := pc.LoreDB.QueryRowContext(ctx,
			`SELECT status FROM entries WHERE id = ?`, p.LeftID,
		).Scan(&priorStatus); err != nil {
			return fmt.Errorf("sleep: consolidate: prior status %s: %w", loreRef(p.LeftID), err)
		}

		// Older (lower id) superseded by newer. Reforge is the
		// canonical primitive: one transaction, supersedes edge,
		// survivor re-embed; never the raw UPDATE the commune fix path
		// performs.
		if err := lore.Reforge(ctx, pc.LoreDB, p.LeftID, p.RightID, pc.now(), pc.Embed); err != nil {
			return fmt.Errorf("sleep: consolidate: reforge %s: %w", target, err)
		}
		supersededThisPass[p.LeftID] = true

		inverse, err := json.Marshal(meldInverse{
			PriorStatus: priorStatus,
			EdgeFrom:    p.RightID,
			EdgeTo:      p.LeftID,
			Relation:    "supersedes",
		})
		if err != nil {
			return fmt.Errorf("sleep: consolidate: marshal inverse: %w", err)
		}
		if err := recordConsolidateOp(ctx, pc, OpMeldExactSupersede, target, detail, string(inverse), true); err != nil {
			return err
		}
		report.OpsApplied++
	}
	return nil
}

// postMeldApprovals posts one approval quest per gated pair (near
// match or kind drift), deduplicated against open quests whose subject
// already names the canonical pair token. Posts over
// Caps.MaxQuestPosts, or with no quest db wired, are journaled as
// deferred.
func (s *ConsolidateStep) postMeldApprovals(ctx context.Context, pc *PassContext, pairs []lore.MeldPair, openSubjects []string, report *StepReport, deferred, suppressed *int) error {
	for _, p := range pairs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sleep: consolidate: cancelled: %w", err)
		}

		token := fmt.Sprintf("%s + %s", loreRef(p.LeftID), loreRef(p.RightID))
		if subjectsMention(openSubjects, token) {
			*suppressed++
			continue
		}

		detail := meldDetail{
			Left:         loreRef(p.LeftID),
			Right:        loreRef(p.RightID),
			LeftProject:  p.LeftProject,
			RightProject: p.RightProject,
			Score:        p.Score,
			KindDrift:    p.KindDrift,
		}
		target := fmt.Sprintf("%s+%s", loreRef(p.LeftID), loreRef(p.RightID))

		if reason, ok := postBlocked(pc, report.OpsPosted); !ok {
			detail.Deferred = true
			detail.Reason = reason
			if err := recordConsolidateOp(ctx, pc, OpNearMeld, target, detail, "", false); err != nil {
				return err
			}
			*deferred++
			continue
		}

		body, err := s.meldQuestBody(ctx, pc, p)
		if err != nil {
			return err
		}
		// Cross-project pairs post to the lower-id entry's project (the
		// quest body says so).
		q, err := quest.Post(ctx, pc.QuestDB, p.LeftProject, quest.PostParams{
			Subject:    "approve meld: " + token,
			Priority:   approvalPriority,
			Epic:       ApprovalEpic,
			Acceptance: body,
			Agent:      approvalAgent,
		})
		if err != nil {
			return fmt.Errorf("sleep: consolidate: post approval %s: %w", target, err)
		}
		detail.Quest = q.ID
		detail.Project = p.LeftProject
		if err := recordConsolidateOp(ctx, pc, OpNearMeld, target, detail, "", false); err != nil {
			return err
		}
		report.OpsPosted++
	}
	return nil
}

// postDemotionApprovals posts one approval quest per severe oath-bloat
// entry. The demotion itself is never applied during sleep; the quest
// names the entry and the command to apply.
func (s *ConsolidateStep) postDemotionApprovals(ctx context.Context, pc *PassContext, demotions []lore.InquestRow, severeBoundary int, openSubjects []string, report *StepReport, deferred, suppressed *int) error {
	for _, e := range demotions {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sleep: consolidate: cancelled: %w", err)
		}

		token := "approve oath demotion: " + loreRef(e.EntryID)
		if subjectsMention(openSubjects, token) {
			*suppressed++
			continue
		}

		detail := demotionDetail{
			Entry:          loreRef(e.EntryID),
			Project:        e.ProjectID,
			WordCount:      e.WordCount,
			SevereBoundary: severeBoundary,
		}

		if reason, ok := postBlocked(pc, report.OpsPosted); !ok {
			detail.Deferred = true
			detail.Reason = reason
			if err := recordConsolidateOp(ctx, pc, OpOathDemotion, loreRef(e.EntryID), detail, "", false); err != nil {
				return err
			}
			*deferred++
			continue
		}

		body := []string{
			fmt.Sprintf("entry: %s (%s) %s", loreRef(e.EntryID), e.ProjectID, sanitizeNoteText(e.Title)),
			fmt.Sprintf("principle is %d words, severe boundary is %d", e.WordCount, severeBoundary),
			fmt.Sprintf("to demote just this entry: lore_update(entry_id=%d, kind=decision)", e.EntryID),
			"to demote every severe principle at once: guild lore commune --fix",
		}
		q, err := quest.Post(ctx, pc.QuestDB, e.ProjectID, quest.PostParams{
			Subject:    token + " (principle exceeds severe boundary)",
			Priority:   approvalPriority,
			Epic:       ApprovalEpic,
			Acceptance: body,
			Agent:      approvalAgent,
		})
		if err != nil {
			return fmt.Errorf("sleep: consolidate: post approval %s: %w", loreRef(e.EntryID), err)
		}
		detail.Quest = q.ID
		if err := recordConsolidateOp(ctx, pc, OpOathDemotion, loreRef(e.EntryID), detail, "", false); err != nil {
			return err
		}
		report.OpsPosted++
	}
	return nil
}

// meldQuestBody builds the [spec] note lines for a meld approval quest:
// both titles, the score, the drift flag, and the one-call commands to
// resolve it either way.
func (s *ConsolidateStep) meldQuestBody(ctx context.Context, pc *PassContext, p lore.MeldPair) ([]string, error) {
	leftTitle, leftKind, err := entryTitleKind(ctx, pc.LoreDB, p.LeftID)
	if err != nil {
		return nil, err
	}
	rightTitle, rightKind, err := entryTitleKind(ctx, pc.LoreDB, p.RightID)
	if err != nil {
		return nil, err
	}

	score := strconv.FormatFloat(p.Score, 'f', -1, 64)
	body := []string{
		fmt.Sprintf("left: %s (%s, %s) %s", loreRef(p.LeftID), p.LeftProject, leftKind, sanitizeNoteText(leftTitle)),
		fmt.Sprintf("right: %s (%s, %s) %s", loreRef(p.RightID), p.RightProject, rightKind, sanitizeNoteText(rightTitle)),
		fmt.Sprintf("similarity score %s, kind drift %v", score, p.KindDrift),
		fmt.Sprintf("to merge: lore_reforge(old_entry_id=%d, new_entry_id=%d) supersedes the older entry", p.LeftID, p.RightID),
		fmt.Sprintf("to archive one side as noise instead: lore_seal(entry_id=%d)", p.LeftID),
	}
	if p.KindDrift {
		body = append(body, fmt.Sprintf("kinds differ (%s vs %s): confirm the surviving kind before applying", leftKind, rightKind))
	}
	if p.LeftProject != p.RightProject {
		body = append(body, fmt.Sprintf("cross-project pair, posted to %s (owner of the older entry)", p.LeftProject))
	}
	return body, nil
}

// postBlocked reports whether the next approval post must be deferred:
// reason is non-empty when blocked. Posting requires a quest db and
// room under Caps.MaxQuestPosts (zero cap means no cap).
func postBlocked(pc *PassContext, posted int) (reason string, ok bool) {
	if pc.QuestDB == nil {
		return reasonNoQuestDB, false
	}
	if pc.Caps.MaxQuestPosts > 0 && posted >= pc.Caps.MaxQuestPosts {
		return reasonQuestPostCap, false
	}
	return "", true
}

// recordConsolidateOp journals one consolidation op with its Classify
// verdict, so every row's policy label is derived from the gate rather
// than hand-assigned.
func recordConsolidateOp(ctx context.Context, pc *PassContext, kind OpKind, target string, detail any, inverse string, applied bool) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("sleep: consolidate: marshal detail: %w", err)
	}
	if _, err := RecordOp(ctx, pc.LoreDB, Op{
		PassID:  pc.PassID,
		Step:    StepNameConsolidate,
		Policy:  Classify(kind),
		Kind:    kind,
		Target:  target,
		Detail:  string(raw),
		Inverse: inverse,
		Applied: applied,
	}); err != nil {
		return fmt.Errorf("sleep: consolidate: journal: %w", err)
	}
	return nil
}

// loadOpenQuestSubjects returns the subject of every open quest
// (status next/blocked/in_progress) across all projects, resolved
// through the canonical quest spec replay so this package does not fork
// the [spec]-note parsing. Approval-quest dedupe matches against these.
func loadOpenQuestSubjects(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT project_id, task_id
		   FROM task_status
		  WHERE status IN ('next', 'blocked', 'in_progress')
		  ORDER BY project_id, task_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("open quests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type ref struct{ project, id string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.project, &r.id); err != nil {
			return nil, fmt.Errorf("open quests scan: %w", err)
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("open quests iterate: %w", err)
	}

	subjects := make([]string, 0, len(refs))
	for _, r := range refs {
		q, err := quest.Load(ctx, db, r.project, r.id)
		if err != nil {
			return nil, fmt.Errorf("open quest %s/%s: %w", r.project, r.id, err)
		}
		if q.Subject != "" {
			subjects = append(subjects, q.Subject)
		}
	}
	return subjects, nil
}

// subjectsMention reports whether any subject contains token at an id
// boundary. Tokens end in an entry id's digits, so the character after
// a match must not be another digit ("LORE-2 + LORE-7" must not match
// a subject naming "LORE-2 + LORE-71").
func subjectsMention(subjects []string, token string) bool {
	for _, s := range subjects {
		if subjectHasToken(s, token) {
			return true
		}
	}
	return false
}

// subjectHasToken implements the boundary-aware contains check for one
// subject.
func subjectHasToken(subject, token string) bool {
	for start := 0; ; {
		idx := strings.Index(subject[start:], token)
		if idx < 0 {
			return false
		}
		end := start + idx + len(token)
		if end >= len(subject) || !isASCIIDigit(subject[end]) {
			return true
		}
		start += idx + 1
	}
}

// isASCIIDigit reports whether c is '0'..'9'.
func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// entryTitleKind loads the title and kind of one lore entry.
func entryTitleKind(ctx context.Context, db *sql.DB, id int64) (title, kind string, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT title, kind FROM entries WHERE id = ?`, id,
	).Scan(&title, &kind)
	if err != nil {
		return "", "", fmt.Errorf("sleep: consolidate: load %s: %w", loreRef(id), err)
	}
	return title, kind, nil
}

// sanitizeNoteText keeps free text safe inside a [spec] note payload:
// the quest spec replay splits payloads on "; ", so embedded "; "
// sequences would shear the note apart on read.
func sanitizeNoteText(s string) string {
	return strings.ReplaceAll(s, "; ", ", ")
}

// loreRef formats a lore entry id for display ("LORE-12").
func loreRef(id int64) string { return fmt.Sprintf("LORE-%d", id) }

// sortPairs orders meld pairs by (LeftID, RightID) so processing order,
// cap consumption, and journal rows are deterministic across runs.
func sortPairs(pairs []lore.MeldPair) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].LeftID != pairs[j].LeftID {
			return pairs[i].LeftID < pairs[j].LeftID
		}
		return pairs[i].RightID < pairs[j].RightID
	})
}
