package quest

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// acceptAndLease posts a quest, accepts it under holder, and writes a lease
// for sessionID that expired ttlAgo before now. It returns the quest id.
// The lease's expires_at is in the past, so the reaper sees it as lapsed.
func acceptAndLease(t *testing.T, db *sql.DB, pid, holder, sessionID string, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	q := mustPost(t, db, pid, PostParams{Subject: "reap target"})
	if _, err := Accept(ctx, db, pid, q.ID, holder); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	// Acquire with a one-second TTL anchored a minute in the past, so the
	// lease is already expired relative to now.
	if err := AcquireLease(ctx, db, pid, q.ID, sessionID, holder, now.Add(-time.Minute), time.Second); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	return q.ID
}

// releasedEventCount returns how many `released` events the quest has.
func releasedEventCount(t *testing.T, db *sql.DB, pid, taskID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_events WHERE project_id = ? AND task_id = ? AND event = ?`,
		pid, taskID, EventReleased).Scan(&n); err != nil {
		t.Fatalf("count released events: %v", err)
	}
	return n
}

// releasedNoteCount returns how many [released]-prefixed notes the quest has.
func releasedNoteCount(t *testing.T, db *sql.DB, pid, taskID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_notes WHERE project_id = ? AND task_id = ? AND note LIKE '[released] %'`,
		pid, taskID).Scan(&n); err != nil {
		t.Fatalf("count released notes: %v", err)
	}
	return n
}

// neverLive is the conservative liveness predicate: no session is live, so
// every expired lease is treated as a crash candidate.
func neverLive(string) bool { return false }

// TestReap_ExpiredZombieClaim_AutoForfeits is the headline acceptance case:
// an expired lease whose session is absent from the registry and whose claim
// is still in_progress under the matching holder is auto-forfeited, a
// released event and [released] note land, and the lease row is deleted.
func TestReap_ExpiredZombieClaim_AutoForfeits(t *testing.T) {
	db, pid := newTestDB(t)
	now := time.Now().UTC()
	taskID := acceptAndLease(t, db, pid, "alice", "sess-1", now)

	res, err := ReapExpiredLeases(context.Background(), db, now, neverLive)
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if res.Forfeited != 1 {
		t.Fatalf("Forfeited = %d, want 1 (result=%+v)", res.Forfeited, res)
	}

	if s := mustStatus(t, db, pid, taskID); s != StatusNext {
		t.Errorf("status = %q, want next after auto-forfeit", s)
	}
	if got := releasedEventCount(t, db, pid, taskID); got != 1 {
		t.Errorf("released events = %d, want 1", got)
	}
	if got := releasedNoteCount(t, db, pid, taskID); got != 1 {
		t.Errorf("[released] notes = %d, want 1", got)
	}
	if got := leaseRowCount(t, db, pid, taskID); got != 0 {
		t.Errorf("lease rows after reap = %d, want 0 (deleted)", got)
	}
}

// TestReap_LiveSession_NoForfeit verifies that an expired lease whose session
// is still live in the registry is left alone (a missed heartbeat, not a
// crash): no forfeit, no released event, and the lease row stays for the
// heartbeat tick to renew.
func TestReap_LiveSession_NoForfeit(t *testing.T) {
	db, pid := newTestDB(t)
	now := time.Now().UTC()
	taskID := acceptAndLease(t, db, pid, "alice", "sess-live", now)

	live := func(id string) bool { return id == "sess-live" }
	res, err := ReapExpiredLeases(context.Background(), db, now, live)
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if res.Forfeited != 0 {
		t.Errorf("Forfeited = %d, want 0 for a live session", res.Forfeited)
	}
	if res.SkippedLive != 1 {
		t.Errorf("SkippedLive = %d, want 1", res.SkippedLive)
	}
	if s := mustStatus(t, db, pid, taskID); s != StatusInProgress {
		t.Errorf("status = %q, want in_progress (untouched)", s)
	}
	if got := releasedEventCount(t, db, pid, taskID); got != 0 {
		t.Errorf("released events = %d, want 0", got)
	}
	if got := leaseRowCount(t, db, pid, taskID); got != 1 {
		t.Errorf("lease rows = %d, want 1 (left for the tick)", got)
	}
}

// TestReap_ClaimReassigned_NoForfeit_OrphanCleared verifies guard (b): when a
// claim has been forfeited and re-accepted by a different holder since the
// lease was written, the holder no longer matches, so the reaper deletes the
// stale lease row (orphan) WITHOUT forfeiting the new holder's live claim.
func TestReap_ClaimReassigned_NoForfeit_OrphanCleared(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	taskID := acceptAndLease(t, db, pid, "alice", "sess-old", now)

	// Alice's claim is released and Bob picks it up: the live claim is now
	// Bob's, but the stale lease still names Alice/sess-old.
	if _, err := Forfeit(ctx, db, pid, taskID, "handing off"); err != nil {
		t.Fatalf("Forfeit (handoff): %v", err)
	}
	if _, err := Accept(ctx, db, pid, taskID, "bob"); err != nil {
		t.Fatalf("Accept (bob): %v", err)
	}

	beforeReleases := releasedEventCount(t, db, pid, taskID)
	res, err := ReapExpiredLeases(ctx, db, now, neverLive)
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if res.Forfeited != 0 {
		t.Errorf("Forfeited = %d, want 0 (claim re-assigned to a new holder)", res.Forfeited)
	}
	if res.OrphansCleared != 1 {
		t.Errorf("OrphansCleared = %d, want 1", res.OrphansCleared)
	}
	// Bob's live claim is untouched.
	if s := mustStatus(t, db, pid, taskID); s != StatusInProgress {
		t.Errorf("status = %q, want in_progress (bob still holds it)", s)
	}
	q := mustLoad(t, db, pid, taskID)
	if q.Owner != "bob" {
		t.Errorf("owner = %q, want bob (reaper must not disturb the re-assigned claim)", q.Owner)
	}
	// No NEW release event from the reaper (only the manual handoff above).
	if got := releasedEventCount(t, db, pid, taskID); got != beforeReleases {
		t.Errorf("released events = %d, want unchanged %d", got, beforeReleases)
	}
	// The orphan lease row is gone.
	if got := leaseRowCount(t, db, pid, taskID); got != 0 {
		t.Errorf("orphan lease rows = %d, want 0 (cleared)", got)
	}
}

// TestReap_FulfilledSinceLease_NoForfeit_OrphanCleared verifies guard (b)
// against a done quest: a claim fulfilled since the lease was written is no
// longer in_progress, so the reaper clears the orphan lease without
// reopening the completed quest.
func TestReap_FulfilledSinceLease_NoForfeit_OrphanCleared(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	taskID := acceptAndLease(t, db, pid, "alice", "sess-done", now)

	if _, err := Fulfill(ctx, db, pid, taskID, "shipped"); err != nil {
		t.Fatalf("Fulfill: %v", err)
	}

	res, err := ReapExpiredLeases(ctx, db, now, neverLive)
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if res.Forfeited != 0 {
		t.Errorf("Forfeited = %d, want 0 (quest already done)", res.Forfeited)
	}
	if res.OrphansCleared != 1 {
		t.Errorf("OrphansCleared = %d, want 1", res.OrphansCleared)
	}
	if s := mustStatus(t, db, pid, taskID); s != StatusDone {
		t.Errorf("status = %q, want done (reaper must not reopen a fulfilled quest)", s)
	}
	if got := leaseRowCount(t, db, pid, taskID); got != 0 {
		t.Errorf("orphan lease rows = %d, want 0", got)
	}
}

// TestReap_NoLeaseRow_NeverTouched is the explicit no-daemon-byte-identical
// guarantee: a claim accepted WITHOUT the daemon writes no lease row, so the
// reaper (which scans task_leases only) never sees it and never forfeits it,
// no matter how long it has been held.
func TestReap_NoLeaseRow_NeverTouched(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Accept with NO lease (the no-daemon accept path writes none).
	q := mustPost(t, db, pid, PostParams{Subject: "no-lease claim"})
	if _, err := Accept(ctx, db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	res, err := ReapExpiredLeases(ctx, db, now, neverLive)
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if res.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0 (no lease rows exist)", res.Scanned)
	}
	if res.Forfeited != 0 {
		t.Errorf("Forfeited = %d, want 0 (a no-lease claim must never be touched)", res.Forfeited)
	}
	if s := mustStatus(t, db, pid, q.ID); s != StatusInProgress {
		t.Errorf("status = %q, want in_progress (no-daemon claim untouched)", s)
	}
}

// TestReap_DoubleFire_Idempotent is the double-fire idempotence acceptance
// case under -race: two overlapping sweeps over the same expired zombie lease
// produce exactly one release. The shared on-disk DB and BEGIN IMMEDIATE
// gating in Forfeit serialize the two sweeps so only one flips the status.
func TestReap_DoubleFire_Idempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quest.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.Migrate(ctx, db, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const pid = "testproj"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
		pid, t.TempDir(), "TASKS.md"); err != nil {
		t.Fatalf("register project: %v", err)
	}

	now := time.Now().UTC()
	taskID := acceptAndLease(t, db, pid, "alice", "sess-1", now)

	// Two sweeps race against their own DB handles to the same file, the
	// way two overlapping daemon ticks would (each reapSeam opens its own
	// handle). Each must observe the same single forfeit outcome in
	// aggregate.
	var forfeited int64
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rdb, err := storage.Open(ctx, path)
			if err != nil {
				t.Errorf("open sweep db: %v", err)
				return
			}
			defer func() { _ = rdb.Close() }()
			res, err := ReapExpiredLeases(ctx, rdb, now, neverLive)
			if err != nil {
				t.Errorf("ReapExpiredLeases: %v", err)
				return
			}
			atomic.AddInt64(&forfeited, int64(res.Forfeited))
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&forfeited); got != 1 {
		t.Fatalf("total Forfeited across two overlapping sweeps = %d, want exactly 1", got)
	}
	if got := releasedEventCount(t, db, pid, taskID); got != 1 {
		t.Errorf("released events = %d, want exactly 1 (idempotent)", got)
	}
	if s := mustStatus(t, db, pid, taskID); s != StatusNext {
		t.Errorf("status = %q, want next", s)
	}
	if got := leaseRowCount(t, db, pid, taskID); got != 0 {
		t.Errorf("lease rows = %d, want 0", got)
	}
}

// TestReap_NilDB_Errors guards the input-validation path.
func TestReap_NilDB_Errors(t *testing.T) {
	if _, err := ReapExpiredLeases(context.Background(), nil, time.Now(), neverLive); err == nil {
		t.Fatal("ReapExpiredLeases(nil db) = nil error, want error")
	}
}
