package quest

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
)

// renewalCount returns how many OPEN quests (next/in_progress/blocked)
// carry the renewal marker for entryID.
func renewalCount(t *testing.T, db *sql.DB, pid string, entryID int64) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_notes n
		 JOIN task_status s
		   ON s.project_id = n.project_id AND s.task_id = n.task_id
		 WHERE n.project_id = ? AND n.note = ?
		   AND s.status IN ('next', 'in_progress', 'blocked')`,
		pid, renewalMarker(entryID),
	).Scan(&n)
	if err != nil {
		t.Fatalf("renewalCount: %v", err)
	}
	return n
}

// setStatus force-writes a task_status row, bypassing lifecycle checks.
// Test-only: lets dedupe tests flip a renewal quest to blocked/done
// without walking the accept/fulfill state machine.
func setStatus(t *testing.T, db *sql.DB, pid, questID string, status Status) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE task_status SET status = ? WHERE project_id = ? AND task_id = ?`,
		string(status), pid, questID,
	); err != nil {
		t.Fatalf("setStatus %s=%s: %v", questID, status, err)
	}
}

func TestPostRenewals_PostsFullQuestShape(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	res, err := PostRenewals(ctx, db, pid, []RenewalCandidate{{
		EntryID:  42,
		Title:    "daemon socket path contract",
		Kind:     "decision",
		FilePath: "docs/contracts/socket.md",
		Reason:   "47d old (valid: 30d)",
	}}, 5, "tester")
	if err != nil {
		t.Fatalf("PostRenewals: %v", err)
	}
	if len(res.Posted) != 1 || len(res.Deduped) != 0 || len(res.OverCap) != 0 {
		t.Fatalf("result = %+v, want exactly 1 posted", res)
	}
	if res.Posted[0].EntryID != 42 {
		t.Errorf("Posted[0].EntryID = %d, want 42", res.Posted[0].EntryID)
	}

	q := mustLoad(t, db, pid, res.Posted[0].QuestID)
	wantSubject := `renew lore: ENTRY-42 "daemon socket path contract" (47d old (valid: 30d))`
	if q.Subject != wantSubject {
		t.Errorf("subject = %q, want %q", q.Subject, wantSubject)
	}
	if q.Priority != RenewalPriority {
		t.Errorf("priority = %q, want %q", q.Priority, RenewalPriority)
	}
	if q.Epic != RenewalEpic {
		t.Errorf("epic = %q, want %q", q.Epic, RenewalEpic)
	}
	if q.Status != StatusNext {
		t.Errorf("status = %q, want next", q.Status)
	}
	if len(q.Files) != 1 || q.Files[0] != "docs/contracts/socket.md" {
		t.Errorf("files = %v, want the entry file_path", q.Files)
	}
	if len(q.Acceptance) == 0 {
		t.Error("acceptance criteria missing")
	}

	// Marker note written with the post.
	if n := renewalCount(t, db, pid, 42); n != 1 {
		t.Errorf("open renewal marker count = %d, want 1", n)
	}
	// `created` event emitted via the Post write sequence.
	var events int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_events
		 WHERE project_id = ? AND task_id = ? AND event = ?`,
		pid, q.ID, EventCreated,
	).Scan(&events); err != nil {
		t.Fatalf("count created events: %v", err)
	}
	if events != 1 {
		t.Errorf("created events = %d, want 1", events)
	}
}

func TestPostRenewals_DedupeLifecycle(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	cand := []RenewalCandidate{{EntryID: 7, Title: "t", Reason: "stale"}}

	first, err := PostRenewals(ctx, db, pid, cand, 5, "")
	if err != nil {
		t.Fatalf("first PostRenewals: %v", err)
	}
	if len(first.Posted) != 1 {
		t.Fatalf("first call posted %d, want 1", len(first.Posted))
	}
	questID := first.Posted[0].QuestID

	// Open (next) blocks a second post.
	second, err := PostRenewals(ctx, db, pid, cand, 5, "")
	if err != nil {
		t.Fatalf("second PostRenewals: %v", err)
	}
	if len(second.Posted) != 0 || len(second.Deduped) != 1 || second.Deduped[0] != 7 {
		t.Fatalf("second result = %+v, want deduped [7]", second)
	}

	// Blocked still counts as open.
	setStatus(t, db, pid, questID, StatusBlocked)
	third, err := PostRenewals(ctx, db, pid, cand, 5, "")
	if err != nil {
		t.Fatalf("third PostRenewals: %v", err)
	}
	if len(third.Posted) != 0 || len(third.Deduped) != 1 {
		t.Fatalf("third result = %+v, want deduped [7]", third)
	}

	// Done releases the dedupe: re-posting is allowed again.
	setStatus(t, db, pid, questID, StatusDone)
	fourth, err := PostRenewals(ctx, db, pid, cand, 5, "")
	if err != nil {
		t.Fatalf("fourth PostRenewals: %v", err)
	}
	if len(fourth.Posted) != 1 || len(fourth.Deduped) != 0 {
		t.Fatalf("fourth result = %+v, want 1 posted after done", fourth)
	}
	if fourth.Posted[0].QuestID == questID {
		t.Error("re-post reused the old quest id")
	}
	if n := renewalCount(t, db, pid, 7); n != 1 {
		t.Errorf("open renewal count = %d, want 1 (old one is done)", n)
	}
}

func TestPostRenewals_CapOldestFirstDeterministic(t *testing.T) {
	db, pid := newTestDB(t)

	// Shuffled input order; selection must be ascending entry id.
	var cands []RenewalCandidate
	for _, id := range []int64{9, 3, 7, 1, 5} {
		cands = append(cands, RenewalCandidate{EntryID: id, Title: "t", Reason: "r"})
	}
	res, err := PostRenewals(context.Background(), db, pid, cands, 2, "")
	if err != nil {
		t.Fatalf("PostRenewals: %v", err)
	}
	if len(res.Posted) != 2 || res.Posted[0].EntryID != 1 || res.Posted[1].EntryID != 3 {
		t.Errorf("Posted = %+v, want entries 1 then 3", res.Posted)
	}
	wantOver := []int64{5, 7, 9}
	if len(res.OverCap) != len(wantOver) {
		t.Fatalf("OverCap = %v, want %v", res.OverCap, wantOver)
	}
	for i, id := range wantOver {
		if res.OverCap[i] != id {
			t.Errorf("OverCap[%d] = %d, want %d", i, res.OverCap[i], id)
		}
	}
}

func TestPostRenewals_CapZeroPostsNothing(t *testing.T) {
	db, pid := newTestDB(t)
	res, err := PostRenewals(context.Background(), db, pid,
		[]RenewalCandidate{{EntryID: 1, Title: "t"}}, 0, "")
	if err != nil {
		t.Fatalf("cap=0 must not error: %v", err)
	}
	if len(res.Posted) != 0 || len(res.OverCap) != 1 {
		t.Errorf("result = %+v, want 0 posted / 1 over-cap", res)
	}
}

func TestPostRenewals_DedupedDoesNotConsumeCap(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	// Entry 1 already has an open renewal quest.
	if _, err := PostRenewals(ctx, db, pid,
		[]RenewalCandidate{{EntryID: 1, Title: "a"}}, 1, ""); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	// cap=1 with [1, 2]: 1 dedupes, leaving the cap slot free for 2.
	res, err := PostRenewals(ctx, db, pid, []RenewalCandidate{
		{EntryID: 1, Title: "a"},
		{EntryID: 2, Title: "b"},
	}, 1, "")
	if err != nil {
		t.Fatalf("PostRenewals: %v", err)
	}
	if len(res.Posted) != 1 || res.Posted[0].EntryID != 2 {
		t.Errorf("Posted = %+v, want entry 2", res.Posted)
	}
	if len(res.Deduped) != 1 || res.Deduped[0] != 1 {
		t.Errorf("Deduped = %v, want [1]", res.Deduped)
	}
	if len(res.OverCap) != 0 {
		t.Errorf("OverCap = %v, want empty", res.OverCap)
	}
}

func TestPostRenewals_EmptyCandidates(t *testing.T) {
	db, pid := newTestDB(t)
	res, err := PostRenewals(context.Background(), db, pid, nil, 5, "")
	if err != nil {
		t.Fatalf("PostRenewals(nil): %v", err)
	}
	if len(res.Posted)+len(res.Deduped)+len(res.OverCap) != 0 {
		t.Errorf("result = %+v, want all empty", res)
	}
}

func TestPostRenewals_InputValidation(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	ok := []RenewalCandidate{{EntryID: 1, Title: "t"}}

	if _, err := PostRenewals(ctx, nil, pid, ok, 1, ""); err == nil {
		t.Error("nil db: want error")
	}
	if _, err := PostRenewals(ctx, db, "  ", ok, 1, ""); err == nil {
		t.Error("empty project: want error")
	}
	if _, err := PostRenewals(ctx, db, pid, ok, -1, ""); err == nil {
		t.Error("negative cap: want error")
	}
	if _, err := PostRenewals(ctx, db, pid,
		[]RenewalCandidate{{EntryID: 0, Title: "t"}}, 1, ""); err == nil {
		t.Error("zero entry id: want error")
	}
}

// TestPostRenewals_Race is the dedupe invariant under contention: two
// goroutines posting overlapping candidate sets must never produce two
// open renewal quests for the same entry. Runs under -race.
func TestPostRenewals_Race(t *testing.T) {
	db, pid := newTestDB(t)

	entries := []int64{1, 2, 3, 4, 5}
	mkCands := func() []RenewalCandidate {
		out := make([]RenewalCandidate, 0, len(entries))
		for _, id := range entries {
			out = append(out, RenewalCandidate{
				EntryID: id,
				Title:   fmt.Sprintf("entry %d", id),
				Reason:  "stale",
			})
		}
		return out
	}

	const G = 2
	var (
		wg      sync.WaitGroup
		start   sync.WaitGroup
		results [G]*RenewalResult
		errs    [G]error
	)
	start.Add(1)
	wg.Add(G)
	for g := 0; g < G; g++ {
		go func(g int) {
			defer wg.Done()
			start.Wait() // release both callers at once
			results[g], errs[g] = PostRenewals(
				context.Background(), db, pid, mkCands(), len(entries), fmt.Sprintf("worker-%d", g))
		}(g)
	}
	start.Done()
	wg.Wait()

	for g, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", g, err)
		}
	}

	// Per entry: exactly one open renewal quest, and exactly one of the
	// two callers posted it (the other deduped or never reached it).
	for _, id := range entries {
		if n := renewalCount(t, db, pid, id); n != 1 {
			t.Errorf("entry %d: open renewal quests = %d, want exactly 1", id, n)
		}
		posted := 0
		for g := 0; g < G; g++ {
			for _, p := range results[g].Posted {
				if p.EntryID == id {
					posted++
				}
			}
		}
		if posted != 1 {
			t.Errorf("entry %d: posted by %d callers, want exactly 1", id, posted)
		}
	}
}
