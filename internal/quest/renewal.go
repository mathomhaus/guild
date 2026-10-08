package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Renewal quests carry a fixed campaign and priority so boards can
// filter the maintenance stream away from feature work. Both are part
// of the wire contract: the values land in [spec] notes in SQLite.
const (
	// RenewalEpic is the campaign every renewal quest is filed under.
	RenewalEpic = "lore-renewal"
	// RenewalPriority is the priority every renewal quest is posted at.
	RenewalPriority Priority = "P3"
)

// RenewalCandidate describes one stale lore entry that should become a
// renewal quest. It is a neutral descriptor rather than a lore type on
// purpose: any staleness detector (a scheduled batch pass over fading
// entries, a watcher pipeline reacting to source-file changes, an
// ad-hoc sweep) can fill it without internal/quest depending on the
// detector's internals.
type RenewalCandidate struct {
	// EntryID is the numeric lore entry id (the N in ENTRY-N). Must be
	// positive; lore ids are minted monotonically, so a lower id is an
	// older entry.
	EntryID int64
	// Title is the entry's title, quoted verbatim in the quest subject.
	Title string
	// Kind is the entry's kind (decision, runbook, ...). Optional;
	// woven into the quest acceptance text when set so the executor
	// knows what shape of claim it is re-verifying.
	Kind string
	// FilePath is the source path the entry cites, when known. Recorded
	// as the quest's files field so the executor lands in the right
	// place.
	FilePath string
	// Reason is the short staleness reason (e.g. "47d old (valid:
	// 30d)"). Quoted in the quest subject; empty defaults to "stale".
	Reason string
}

// RenewalPost records one successfully posted renewal quest.
type RenewalPost struct {
	QuestID string
	EntryID int64
}

// RenewalResult reports what PostRenewals did with every candidate so
// callers can journal the outcome.
type RenewalResult struct {
	// Posted lists the minted renewal quests in posting order (ascending
	// entry id).
	Posted []RenewalPost
	// Deduped lists entry ids skipped because an open renewal quest for
	// the entry already exists.
	Deduped []int64
	// OverCap lists entry ids skipped because maxPosts was already
	// reached when their turn came.
	OverCap []int64
}

// PostRenewals turns stale lore entries into capped, deduplicated
// renewal quests: knowledge decay generates work instead of silently
// rotting.
//
// It is the shared lore-to-quest decay path. Callers detect staleness
// and hand the candidates here; PostRenewals owns selection, dedupe,
// and posting. It makes no scheduling or daemon assumptions, so it is
// equally callable from a periodic batch pass over fading entries, a
// watcher pipeline reacting to source changes, or a one-shot CLI sweep,
// including concurrently from several of those at once.
//
// Semantics:
//
//   - Selection is deterministic: candidates are processed oldest entry
//     first (ascending EntryID), regardless of input order.
//   - At most maxPosts quests are posted per call. maxPosts=0 posts
//     nothing and is not an error; a negative maxPosts is an error.
//     Candidates left unexamined once the cap is hit are reported in
//     RenewalResult.OverCap.
//   - Dedupe: a candidate is skipped (RenewalResult.Deduped) when an
//     open quest (status next, in_progress, or blocked) already carries
//     the entry's NotePrefixRenewal marker note. A done renewal quest
//     does not block re-posting. Deduped candidates do not consume the
//     cap.
//   - Each post goes through the standard Post write sequence (monotonic
//     QUEST-N minting under BEGIN IMMEDIATE, [spec] notes, created event
//     in task_events) plus the marker note, all in one transaction, so a
//     crash cannot leave a renewal quest without its dedupe marker.
//     Because the dedupe scan runs inside the same BEGIN IMMEDIATE
//     transaction, concurrent callers serialize on the write lock and
//     see each other's committed markers: no entry ever ends up with two
//     open renewal quests.
//
// Each posted quest has subject `renew lore: ENTRY-N "<title>"
// (<reason>)`, campaign RenewalEpic, priority RenewalPriority, the
// entry's file path (when set) as its files field, and acceptance
// criteria instructing the executor to re-verify the entry against its
// source and then update, reforge, or seal it via the lore tools.
//
// Each candidate commits in its own transaction. On a mid-batch error
// the returned RenewalResult still reflects the posts already
// committed, alongside the error.
func PostRenewals(ctx context.Context, db *sql.DB, projectID string, candidates []RenewalCandidate, maxPosts int, agent string) (*RenewalResult, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: post renewals: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: post renewals: empty project_id")
	}
	if maxPosts < 0 {
		return nil, fmt.Errorf("quest: post renewals: negative cap %d", maxPosts)
	}
	for i := range candidates {
		if candidates[i].EntryID <= 0 {
			return nil, fmt.Errorf("quest: post renewals: candidate %d: entry id must be positive, got %d",
				i, candidates[i].EntryID)
		}
	}

	// Deterministic selection: oldest entry first. Stable so duplicate
	// entry ids keep their input order (the later duplicate dedupes
	// against the earlier one's freshly committed marker).
	ordered := make([]RenewalCandidate, len(candidates))
	copy(ordered, candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].EntryID < ordered[j].EntryID
	})

	agent = agentOrDefault(agent)
	res := &RenewalResult{}
	for i := range ordered {
		cand := &ordered[i]
		if len(res.Posted) >= maxPosts {
			res.OverCap = append(res.OverCap, cand.EntryID)
			continue
		}
		q, err := postOneRenewal(ctx, db, projectID, cand, agent)
		if err != nil {
			return res, err
		}
		if q == nil {
			res.Deduped = append(res.Deduped, cand.EntryID)
			continue
		}
		res.Posted = append(res.Posted, RenewalPost{QuestID: q.ID, EntryID: cand.EntryID})
	}
	return res, nil
}

// postOneRenewal posts the renewal quest for cand inside one BEGIN
// IMMEDIATE transaction: the dedupe scan, the Post write sequence, and
// the marker note commit (or roll back) together. Returns (nil, nil)
// when an open renewal quest for the entry already exists.
func postOneRenewal(ctx context.Context, db *sql.DB, projectID string, cand *RenewalCandidate, agent string) (*Quest, error) {
	marker := renewalMarker(cand.EntryID)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	conn, rollback, err := beginImmediate(ctx, db, "post renewal")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	dup, err := openRenewalExists(ctx, conn, projectID, marker)
	if err != nil {
		return nil, err
	}
	if dup {
		// Read-only so far; the deferred ROLLBACK releases the lock.
		return nil, nil
	}

	params := renewalPostParams(cand)
	params.Agent = agent
	q, err := postTx(ctx, conn, projectID, &params, agent, now)
	if err != nil {
		return nil, err
	}
	if err := insertSpecNote(ctx, conn, projectID, q.ID, agent, now, marker); err != nil {
		return nil, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("quest: post renewal: commit: %w", err)
	}
	committed = true
	return q, nil
}

// renewalMarker returns the exact task_notes marker text for entryID.
// The dedupe scan matches it byte-for-byte, so writer and reader must
// agree on the full string, not just the prefix.
func renewalMarker(entryID int64) string {
	return fmt.Sprintf("%sENTRY-%d", NotePrefixRenewal, entryID)
}

// openRenewalExists reports whether any open quest (status next,
// in_progress, or blocked) in projectID carries the given renewal
// marker note. Runs inside the caller's transaction so the answer
// reflects the serialized committed state, not a stale snapshot.
func openRenewalExists(ctx context.Context, tx dbTx, projectID, marker string) (bool, error) {
	const sqlQ = `SELECT 1 FROM task_notes n
	              JOIN task_status s
	                ON s.project_id = n.project_id AND s.task_id = n.task_id
	              WHERE n.project_id = ? AND n.note = ?
	                AND s.status IN ('next', 'in_progress', 'blocked')
	              LIMIT 1`
	var one int
	err := tx.QueryRowContext(ctx, sqlQ, projectID, marker).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("quest: renewal dedupe check: %w", err)
	}
	return true, nil
}

// renewalPostParams builds the PostParams for cand's renewal quest:
// subject `renew lore: ENTRY-N "<title>" (<reason>)`, campaign
// RenewalEpic, priority RenewalPriority, the entry's file path as the
// files field when set, and acceptance criteria that route the executor
// through re-verification before any lore mutation. The renewal quest
// itself is the approval gate in front of destructive lore operations
// (reforge/seal), which is why the criteria insist on citing what was
// checked.
func renewalPostParams(cand *RenewalCandidate) PostParams {
	entry := fmt.Sprintf("ENTRY-%d", cand.EntryID)
	reason := strings.TrimSpace(cand.Reason)
	if reason == "" {
		reason = "stale"
	}

	ref := entry
	if k := strings.TrimSpace(cand.Kind); k != "" {
		ref = fmt.Sprintf("%s (kind %s)", entry, k)
	}
	verify := fmt.Sprintf(
		"Re-verify %s against its current source and cite what was checked", ref)
	if fp := strings.TrimSpace(cand.FilePath); fp != "" {
		verify = fmt.Sprintf(
			"Re-verify %s against the current state of %s and cite what was checked", ref, fp)
	}

	params := PostParams{
		Subject:  fmt.Sprintf("renew lore: %s %q (%s)", entry, cand.Title, reason),
		Priority: RenewalPriority,
		Epic:     RenewalEpic,
		Acceptance: []string{
			verify,
			fmt.Sprintf("If the claim still holds, refresh %s via lore update"+
				" (or lore reforge when the summary needs a rewrite); if it is obsolete, seal it", entry),
			"Journal the verification outcome on this quest before fulfilling it",
		},
	}
	if fp := strings.TrimSpace(cand.FilePath); fp != "" {
		params.Files = []string{fp}
	}
	return params
}
