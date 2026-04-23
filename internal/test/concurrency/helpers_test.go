// Package concurrency_test holds the multi-agent stress suite that
// validates guild's "atomic claims, no collisions" claim at the SQL
// layer. Each file exercises one scenario from QUEST-179 / LORE-299.
//
// Approach A (this file set): N goroutines inside one process hit the
// same SQLite DB through guild's real Accept / Post / Fulfill /
// Inscribe / LinkEntries code paths. This is the CI-gateable form:
// fast (whole suite runs in a few seconds), and runs under `go test
// -race` on every commit.
//
// Approach B (subprocess MCP-level contention) is intentionally
// deferred to a follow-up quest — see RESULT artifact for the
// rationale. The SQL-level suite catches every class of race the
// subprocess form would, because the subprocess agents ultimately
// converge on the same Accept/Fulfill/Inscribe code via stdio.
package concurrency_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/storage"
)

// testProjectID is the dummy project every sub-scenario registers.
const testProjectID = "concproj"

// agentCounts is the contention-N matrix required by LORE-299
// acceptance (2, 10, 100 concurrent agents). Each scenario runs once
// per N via a table-driven t.Run subtest.
var agentCounts = []int{2, 10, 100}

// newSharedDB opens a per-test file-backed SQLite DB, applies the full
// guild migration set, and registers the dummy project. The DB has
// WAL + busy_timeout=5000 via storage.Open — the same pragmas every
// real guild process uses — so contention behavior observed here is
// production-faithful.
//
// We deliberately use a file-backed DB under t.TempDir() (NOT
// ":memory:") because :memory: gives each pooled connection a
// distinct in-memory DB, which would defeat the whole point of a
// contention test. The same rationale is captured in
// internal/quest/testhelpers_test.go (newTestDB).
func newSharedDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrency.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := storage.Migrate(ctx, db, ""); err != nil {
		t.Fatalf("storage.Migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
		testProjectID, t.TempDir(), "TASKS.md",
	); err != nil {
		t.Fatalf("register project: %v", err)
	}
	return db
}

// mustPostQuest posts a quest with the given subject and returns its
// id. Fails the test on any Post error.
func mustPostQuest(t *testing.T, db *sql.DB, subject string) string {
	t.Helper()
	q, err := quest.Post(context.Background(), db, testProjectID, quest.PostParams{
		Subject: subject,
	})
	if err != nil {
		t.Fatalf("Post %q: %v", subject, err)
	}
	return q.ID
}

// mustInscribeAncestor inserts a decision-kind ancestor entry and
// returns its id. Used by the informs-to-same-ancestor scenario.
func mustInscribeAncestor(t *testing.T, db *sql.DB, title string) int64 {
	t.Helper()
	res, err := lore.Inscribe(context.Background(), db, &lore.InscribeParams{
		ProjectID: testProjectID,
		Kind:      lore.KindDecision,
		Title:     title,
		Summary:   "ancestor for concurrency informs test — describes prior art the descendants cite.",
		Topic:     "concurrency",
		NoWarn:    true,
	})
	if err != nil {
		t.Fatalf("Inscribe ancestor %q: %v", title, err)
	}
	return res.Entry.ID
}

// mustAcceptQuest accepts taskID as owner and fails on any error that
// isn't ErrAlreadyClaimed. Callers that expect race losses use the
// raw quest.Accept directly.
func mustAcceptQuest(t *testing.T, db *sql.DB, taskID, owner string) {
	t.Helper()
	if _, err := quest.Accept(context.Background(), db, testProjectID, taskID, owner); err != nil {
		t.Fatalf("Accept %s as %s: %v", taskID, owner, err)
	}
}

// raceResult accumulates per-goroutine outcomes of a contended call.
// All fields are atomic int64s so callers can tally via atomic.AddInt64
// without a mutex.
type raceResult struct {
	successes int64
	claimLoss int64 // only used by Accept scenarios
	other     int64
}

// runBarrier fans N goroutines out behind a single channel-close
// barrier and waits for them to finish. The closure receives the
// goroutine index; returning (success=true, claimLoss=false, err=nil)
// logs a success; (success=false, claimLoss=true, err=nil) logs a
// race loss; any non-nil err that is NOT a claim loss logs as
// "other" and surfaces a t.Errorf.
//
// Channel-close is used as the start signal (not sync.WaitGroup.Wait)
// because close(chan) releases ALL waiters in a single scheduler
// pass — the closest deterministic primitive Go exposes to a
// pthread_barrier_t. No time.Sleep anywhere in the harness.
func runBarrier(
	t *testing.T,
	n int,
	fn func(i int) (success, claimLoss bool, err error),
) raceResult {
	t.Helper()
	var (
		wg  sync.WaitGroup
		res raceResult
	)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start // all goroutines released by close(start) below
			ok, lost, err := fn(i)
			switch {
			case err != nil && !lost:
				atomic.AddInt64(&res.other, 1)
				t.Errorf("worker %d: unexpected err: %v", i, err)
			case lost:
				atomic.AddInt64(&res.claimLoss, 1)
			case ok:
				atomic.AddInt64(&res.successes, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return res
}

// ownerName returns the canonical worker owner label used by Accept
// scenarios. Exposed as a helper so failure logs across scenarios
// stay consistent.
func ownerName(i int) string { return fmt.Sprintf("worker-%d", i) }
