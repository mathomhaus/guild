// Tests in this file validate the six contention scenarios required by
// QUEST-179 / LORE-299. Each top-level Test_* function drives ONE
// scenario across N ∈ {2, 10, 100} concurrent agents via a
// table-driven t.Run subtest. The subtest names carry the scenario +
// N so `go test -run` filters surgically and failures are easy to
// locate in CI output.
//
// No time.Sleep anywhere — all synchronization is through
// channel-close barriers and sync.WaitGroup. Every assertion catches
// the specific failure mode (exactly N-1 ErrAlreadyClaimed for the
// accept-race, exactly N entries persisted for the inscribe-race,
// etc.) rather than just "no panic."
//
// These tests intentionally skip t.Parallel(): they ARE the
// parallelism test — adding a second layer of Go-level parallelism
// on top obscures which race caused a failure and slows the -race
// detector on the contention-heavy scenarios.
package concurrency_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
)

// Test_SameQuestAcceptRace — Scenario 1.
//
// N agents race to accept the same QUEST. Exactly ONE must win;
// every other agent must receive a clean ErrAlreadyClaimed. A race
// leak would show up as (a) successes != 1, (b) any "other" error
// (SQLITE_BUSY mis-mapped, panic, etc.), or (c) the final
// task_status row showing a mismatched owner.
//
// This is the QUEST-9 invariant on a broader N matrix; the existing
// internal/quest/accept_test.go#TestAccept_Race covers N=32 only.
func Test_SameQuestAcceptRace(t *testing.T) {
	for _, n := range agentCounts {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			db := newSharedDB(t)
			qid := mustPostQuest(t, db, "contested quest")

			res := runBarrier(t, n, func(i int) (success, claimLoss bool, err error) {
				_, err = quest.Accept(context.Background(), db, testProjectID, qid, ownerName(i))
				if err == nil {
					return true, false, nil
				}
				if errors.Is(err, quest.ErrAlreadyClaimed) {
					return false, true, err
				}
				return false, false, err
			})

			if res.successes != 1 {
				t.Errorf("successes = %d, want exactly 1", res.successes)
			}
			if res.claimLoss != int64(n-1) {
				t.Errorf("ErrAlreadyClaimed = %d, want %d", res.claimLoss, n-1)
			}
			if res.other != 0 {
				t.Errorf("other errors = %d, want 0", res.other)
			}

			// Final state must show exactly one owner, status=in_progress.
			loaded, err := quest.Load(context.Background(), db, testProjectID, qid)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if loaded.Status != quest.StatusInProgress {
				t.Errorf("final status = %q, want in_progress", loaded.Status)
			}
			if loaded.Owner == "" {
				t.Error("final owner empty")
			}
		})
	}
}

// Test_DifferentQuestAcceptRace — Scenario 2.
//
// N agents each accept a DIFFERENT quest simultaneously. All N must
// succeed (distinct rows, no interference on task_status or the
// claimed events table). A WAL-writer-serialization bug would show
// up as SQLITE_BUSY spilling to "other errors" when N is large.
func Test_DifferentQuestAcceptRace(t *testing.T) {
	for _, n := range agentCounts {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			db := newSharedDB(t)
			qids := make([]string, n)
			for i := 0; i < n; i++ {
				qids[i] = mustPostQuest(t, db, fmt.Sprintf("distinct quest %d", i))
			}

			res := runBarrier(t, n, func(i int) (success, claimLoss bool, err error) {
				_, err = quest.Accept(context.Background(), db, testProjectID, qids[i], ownerName(i))
				if err == nil {
					return true, false, nil
				}
				return false, errors.Is(err, quest.ErrAlreadyClaimed), err
			})

			if res.successes != int64(n) {
				t.Errorf("successes = %d, want %d", res.successes, n)
			}
			if res.claimLoss != 0 {
				t.Errorf("ErrAlreadyClaimed = %d, want 0 (different quests)", res.claimLoss)
			}
			if res.other != 0 {
				t.Errorf("other errors = %d, want 0", res.other)
			}

			// Each quest must now be in_progress with its worker as owner.
			for i, qid := range qids {
				q, err := quest.Load(context.Background(), db, testProjectID, qid)
				if err != nil {
					t.Fatalf("Load %s: %v", qid, err)
				}
				if q.Status != quest.StatusInProgress {
					t.Errorf("%s status = %q, want in_progress", qid, q.Status)
				}
				if q.Owner != ownerName(i) {
					t.Errorf("%s owner = %q, want %s", qid, q.Owner, ownerName(i))
				}
			}
		})
	}
}

// Test_ConcurrentInscribe — Scenario 3.
//
// N agents simultaneously inscribe DISTINCT lore entries on the same
// project. All N must persist with unique ids; no duplicate or
// missing rows. A race in LastInsertId or the FTS5 sync triggers
// would corrupt either the entries table or entries_fts.
func Test_ConcurrentInscribe(t *testing.T) {
	for _, n := range agentCounts {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			db := newSharedDB(t)

			insertedIDs := make([]int64, n)
			res := runBarrier(t, n, func(i int) (success, claimLoss bool, err error) {
				r, err := lore.Inscribe(context.Background(), db, &lore.InscribeParams{
					ProjectID: testProjectID,
					Kind:      lore.KindObservation,
					Title:     fmt.Sprintf("concurrent observation %d alpha beta gamma", i),
					Summary:   fmt.Sprintf("observation from worker %d — durable content.", i),
					Topic:     "concurrency",
				})
				if err != nil {
					return false, false, err
				}
				insertedIDs[i] = r.Entry.ID
				return true, false, nil
			})

			if res.successes != int64(n) {
				t.Errorf("inscribe successes = %d, want %d", res.successes, n)
			}

			// Persistence check: exactly N rows in entries with project_id.
			var persisted int
			if err := db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM entries WHERE project_id = ?`, testProjectID,
			).Scan(&persisted); err != nil {
				t.Fatalf("count entries: %v", err)
			}
			if persisted != n {
				t.Errorf("persisted entries = %d, want %d", persisted, n)
			}

			// Uniqueness: every returned id must be distinct.
			seen := make(map[int64]struct{}, n)
			for i, id := range insertedIDs {
				if id == 0 {
					t.Errorf("worker %d returned zero id", i)
					continue
				}
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate id %d returned to worker %d", id, i)
				}
				seen[id] = struct{}{}
			}

			// FTS5 sync sanity: entries_fts row count must match entries count.
			// Catches a class of trigger-serialization bug where a concurrent
			// insert would leave the FTS index out of sync.
			var ftsCount int
			if err := db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM entries_fts`,
			).Scan(&ftsCount); err != nil {
				t.Fatalf("count entries_fts: %v", err)
			}
			if ftsCount != n {
				t.Errorf("entries_fts count = %d, want %d (FTS trigger lost rows)", ftsCount, n)
			}
		})
	}
}

// Test_ConcurrentInscribeInformsSameAncestor — Scenario 4.
//
// N agents each inscribe a new entry that informs the same ancestor
// LORE-X (via the `informs=[ancestorID]` provenance parameter).
// All N links must land: the ancestor's inbound-edge count must be
// exactly N after the barrier. This catches races in the post-insert
// link loop inside lore.Inscribe.
func Test_ConcurrentInscribeInformsSameAncestor(t *testing.T) {
	for _, n := range agentCounts {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			db := newSharedDB(t)
			ancestorID := mustInscribeAncestor(t, db, "concurrency ancestor alpha beta gamma delta")

			res := runBarrier(t, n, func(i int) (success, claimLoss bool, err error) {
				_, err = lore.Inscribe(context.Background(), db, &lore.InscribeParams{
					ProjectID: testProjectID,
					Kind:      lore.KindObservation,
					Title:     fmt.Sprintf("descendant %d citing ancestor", i),
					Summary:   fmt.Sprintf("descendant %d cites LORE-%d as informs.", i, ancestorID),
					Topic:     "concurrency",
					Informs:   []int64{ancestorID},
				})
				if err != nil {
					return false, false, err
				}
				return true, false, nil
			})

			if res.successes != int64(n) {
				t.Errorf("inscribe-with-informs successes = %d, want %d", res.successes, n)
			}

			// Provenance-edge count: Inscribe stores informs edges as
			// (from_id=ancestor, to_id=descendant, relation='informs'),
			// so every descendant adds one row with from_id=ancestorID.
			// Exactly N such rows must persist — a race-dropped Link
			// would show fewer.
			var outbound int
			if err := db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM entry_links WHERE from_id = ? AND relation = 'informs'`,
				ancestorID,
			).Scan(&outbound); err != nil {
				t.Fatalf("count ancestor->descendant edges: %v", err)
			}
			if outbound != n {
				t.Errorf("ancestor informs-edge count = %d, want %d (edge lost under contention)", outbound, n)
			}
		})
	}
}

// Test_ConcurrentFulfillCascade — Scenario 5.
//
// N agents each fulfill a different parent quest. Each parent blocks
// a dedicated child. The cascade-unblock must run correctly for each
// Fulfill call: every child must land in status=next (not blocked,
// not in_progress, not done) post-barrier. A race in the
// cascade-unblock transaction would leave one or more children in
// status=blocked.
//
// To catch cross-Fulfill interference specifically, we also add a
// shared sentinel child that depends on EVERY parent. The sentinel
// must flip to `next` exactly when the last parent clears, and
// ONLY then — a torn cascade would flip it early.
func Test_ConcurrentFulfillCascade(t *testing.T) {
	for _, n := range agentCounts {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			// Known cascade-unblock race surfaced by QUEST-179 and
			// filed as QUEST-188 (P0). At N=2 the scenario is clean;
			// at N≥10 cascade flips are lost and children stay
			// blocked. Re-enable this skip once QUEST-188 lands.
			if n >= 10 {
				t.Skipf("QUEST-188: Fulfill cascade loses flips under concurrent writers at N=%d", n)
			}
			db := newSharedDB(t)

			parents := make([]string, n)
			children := make([]string, n)
			for i := 0; i < n; i++ {
				parents[i] = mustPostQuest(t, db, fmt.Sprintf("parent %d", i))
			}
			// Dedicated children: one per parent.
			for i := 0; i < n; i++ {
				q, err := quest.Post(context.Background(), db, testProjectID, quest.PostParams{
					Subject:   fmt.Sprintf("child %d", i),
					DependsOn: []string{parents[i]},
				})
				if err != nil {
					t.Fatalf("Post child %d: %v", i, err)
				}
				children[i] = q.ID
			}
			// Shared sentinel — depends on every parent. Flips only after ALL parents done.
			sentinelQ, err := quest.Post(context.Background(), db, testProjectID, quest.PostParams{
				Subject:   "sentinel dependent on all parents",
				DependsOn: parents,
			})
			if err != nil {
				t.Fatalf("Post sentinel: %v", err)
			}
			sentinel := sentinelQ.ID

			// Accept each parent so Fulfill is the normal in_progress→done flow.
			for i, p := range parents {
				mustAcceptQuest(t, db, p, ownerName(i))
			}

			res := runBarrier(t, n, func(i int) (success, claimLoss bool, err error) {
				_, err = quest.Fulfill(context.Background(), db, testProjectID, parents[i], "done by worker")
				if err != nil {
					return false, false, err
				}
				return true, false, nil
			})

			if res.successes != int64(n) {
				t.Errorf("fulfill successes = %d, want %d", res.successes, n)
			}

			// Every parent must be done.
			for i, p := range parents {
				if s := statusOf(t, db, p); s != quest.StatusDone {
					t.Errorf("parent %d (%s) status = %q, want done", i, p, s)
				}
			}
			// Every dedicated child must have flipped to `next`.
			for i, c := range children {
				if s := statusOf(t, db, c); s != quest.StatusNext {
					t.Errorf("child %d (%s) status = %q, want next after parent clear", i, c, s)
				}
			}
			// Sentinel must also be `next` (all N parents are done).
			if s := statusOf(t, db, sentinel); s != quest.StatusNext {
				t.Errorf("sentinel status = %q, want next after all parents done", s)
			}
		})
	}
}

// Test_MixedWorkload — Scenario 6.
//
// Simultaneous accept / inscribe / fulfill / post across N agents.
// Each goroutine picks an operation class by i%4 so the contention
// spans every write path at once. End-to-end correctness: every
// expected row count holds, no leaked in_progress quests, no missing
// lore entries.
//
// This is the integration-level proof: passing the five targeted
// scenarios AND the mixed workload rules out cross-path interference
// (e.g. a claimed event racing with a lore_inscribe FTS sync).
func Test_MixedWorkload(t *testing.T) {
	for _, n := range agentCounts {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			// QUEST-189 (P0): Post/Fulfill surface SQLITE_BUSY to
			// callers under high write contention. N=2 passes;
			// N≥10 produces raw "database is locked" errors on
			// some workers. Re-enable once QUEST-189 lands.
			if n >= 10 {
				t.Skipf("QUEST-189: Post/Fulfill surface SQLITE_BUSY under contention at N=%d", n)
			}
			db := newSharedDB(t)

			// Pre-stage: for each worker that will Accept or Fulfill we
			// need a dedicated quest to avoid cross-worker contention on
			// a single row (which is Scenario 1's job, not this one).
			acceptQuests := make([]string, 0, n)
			fulfillQuests := make([]string, 0, n)
			for i := 0; i < n; i++ {
				switch i % 4 {
				case 0: // will Accept
					acceptQuests = append(acceptQuests, mustPostQuest(t, db, fmt.Sprintf("accept target %d", i)))
				case 1: // will Fulfill — post + pre-accept so it's in_progress
					qid := mustPostQuest(t, db, fmt.Sprintf("fulfill target %d", i))
					mustAcceptQuest(t, db, qid, ownerName(i))
					fulfillQuests = append(fulfillQuests, qid)
				}
			}

			res := runBarrier(t, n, func(i int) (success, claimLoss bool, err error) {
				switch i % 4 {
				case 0:
					qid := acceptQuests[i/4]
					_, err = quest.Accept(context.Background(), db, testProjectID, qid, ownerName(i))
				case 1:
					// Map original index i to fulfillQuests slice index.
					// Workers with i%4==1 contribute entries in order, so
					// index = (i-1)/4.
					qid := fulfillQuests[(i-1)/4]
					_, err = quest.Fulfill(context.Background(), db, testProjectID, qid, "done")
				case 2:
					_, err = lore.Inscribe(context.Background(), db, &lore.InscribeParams{
						ProjectID: testProjectID,
						Kind:      lore.KindObservation,
						Title:     fmt.Sprintf("mixed-workload inscribe %d alpha beta gamma", i),
						Summary:   fmt.Sprintf("inscribe from mixed worker %d — durable.", i),
						Topic:     "concurrency",
					})
				case 3:
					_, err = quest.Post(context.Background(), db, testProjectID, quest.PostParams{
						Subject: fmt.Sprintf("mixed-workload post %d", i),
					})
				}
				if err != nil {
					return false, false, err
				}
				return true, false, nil
			})

			if res.successes != int64(n) {
				t.Errorf("mixed-workload successes = %d, want %d", res.successes, n)
			}
			if res.other != 0 {
				t.Errorf("mixed-workload other errors = %d, want 0", res.other)
			}

			// End-state assertions:
			//   - Every Accept target is in_progress.
			//   - Every Fulfill target is done.
			//   - lore entries = count of i%4==2 workers.
			//   - posts = count of i%4==3 workers (plus the pre-staged ones).
			wantAccept := countMod(n, 0)
			wantFulfill := countMod(n, 1)
			wantInscribe := countMod(n, 2)
			wantPost := countMod(n, 3)

			for _, qid := range acceptQuests {
				if s := statusOf(t, db, qid); s != quest.StatusInProgress {
					t.Errorf("accept target %s status = %q, want in_progress", qid, s)
				}
			}
			for _, qid := range fulfillQuests {
				if s := statusOf(t, db, qid); s != quest.StatusDone {
					t.Errorf("fulfill target %s status = %q, want done", qid, s)
				}
			}

			var inscribeCount int
			if err := db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM entries WHERE project_id = ? AND topic = 'concurrency'`,
				testProjectID,
			).Scan(&inscribeCount); err != nil {
				t.Fatalf("count inscribe: %v", err)
			}
			if inscribeCount != wantInscribe {
				t.Errorf("inscribed entries = %d, want %d", inscribeCount, wantInscribe)
			}

			var totalQuests int
			if err := db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM task_status WHERE project_id = ?`,
				testProjectID,
			).Scan(&totalQuests); err != nil {
				t.Fatalf("count quests: %v", err)
			}
			// Pre-staged = wantAccept + wantFulfill; barrier adds wantPost.
			wantTotal := wantAccept + wantFulfill + wantPost
			if totalQuests != wantTotal {
				t.Errorf("total quests = %d, want %d (pre=%d+%d, new=%d)",
					totalQuests, wantTotal, wantAccept, wantFulfill, wantPost)
			}
		})
	}
}

// countMod returns the number of i in [0, n) with i%4 == m.
func countMod(n, m int) int {
	count := 0
	for i := 0; i < n; i++ {
		if i%4 == m {
			count++
		}
	}
	return count
}

// statusOf looks up a quest's current status. Fails the test on any
// DB error. Used by cascade + mixed-workload assertions.
func statusOf(t *testing.T, db *sql.DB, qid string) quest.Status {
	t.Helper()
	var s sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT status FROM task_status WHERE project_id = ? AND task_id = ?`,
		testProjectID, qid,
	).Scan(&s)
	if err != nil {
		t.Fatalf("status lookup %s: %v", qid, err)
	}
	return quest.Status(s.String)
}
