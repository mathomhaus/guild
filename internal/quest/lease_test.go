package quest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/storage"
)

// newLeaseTestDB is like newTestDB but also returns the on-disk path, so
// the accept-handler tests can open a fresh *sql.DB per invocation. The
// quest_accept handler defer-closes whatever OpenDB returns; handing it
// the shared assertion handle would close the pool out from under the
// test (and under the 32 racing goroutines in the lease race test).
func newLeaseTestDB(t *testing.T) (db *sql.DB, projectID, path string) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "quest.db")
	var err error
	db, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if migrateErr := storage.Migrate(ctx, db, ""); migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
		"testproj", t.TempDir(), "TASKS.md",
	); err != nil {
		t.Fatalf("register project: %v", err)
	}
	projectID = "testproj"
	return
}

// fixedNow returns a deterministic clock for expiry math.
func fixedNow(t string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, t)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return parsed }
}

// leaseRowCount returns the number of task_leases rows for a quest.
func leaseRowCount(t *testing.T, db *sql.DB, pid, taskID string) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_leases WHERE project_id = ? AND task_id = ?`,
		pid, taskID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("leaseRowCount %s: %v", taskID, err)
	}
	return n
}

// loadLease returns the single lease row for a quest, failing if absent.
func loadLease(t *testing.T, db *sql.DB, pid, taskID string) Lease {
	t.Helper()
	var l Lease
	err := db.QueryRowContext(context.Background(),
		`SELECT project_id, task_id, session_id, holder, acquired_at, heartbeat_at, expires_at
		 FROM task_leases WHERE project_id = ? AND task_id = ?`,
		pid, taskID,
	).Scan(&l.ProjectID, &l.TaskID, &l.SessionID, &l.Holder,
		&l.AcquiredAt, &l.HeartbeatAt, &l.ExpiresAt)
	if err != nil {
		t.Fatalf("loadLease %s: %v", taskID, err)
	}
	return l
}

// TestMigration011_TaskLeasesMaterialized asserts the migration 011 table
// exists with the expected columns after the full migration corpus runs.
// newTestDB applies every embedded migration (001-011) against a tmpdir
// file DB and registers a project, so this also proves 011 coexists with
// the live 001-010 schema (the "applies on an existing DB" acceptance).
func TestMigration011_TaskLeasesMaterialized(t *testing.T) {
	db, _ := newTestDB(t)

	var kind string
	err := db.QueryRowContext(context.Background(),
		`SELECT type FROM sqlite_master WHERE name = 'task_leases'`,
	).Scan(&kind)
	if err != nil {
		t.Fatalf("task_leases not present after migrate: %v", err)
	}
	if kind != "table" {
		t.Errorf("task_leases type = %q, want table", kind)
	}

	wantCols := map[string]bool{
		"project_id": false, "task_id": false, "session_id": false,
		"holder": false, "acquired_at": false, "heartbeat_at": false,
		"expires_at": false,
	}
	rows, err := db.QueryContext(context.Background(),
		`SELECT name FROM pragma_table_info('task_leases')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan col: %v", err)
		}
		if _, ok := wantCols[n]; ok {
			wantCols[n] = true
		}
	}
	for col, seen := range wantCols {
		if !seen {
			t.Errorf("task_leases missing column %q", col)
		}
	}
}

// TestMigration011_AppliesOnPopulatedDB proves 011 is additive and safe
// on a DB that already carries quest data: post + accept a quest (writing
// task_status/notes/events under the full schema), then write and read a
// lease row. If 011 conflicted with the existing schema this would fail.
func TestMigration011_AppliesOnPopulatedDB(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "populated"})
	if _, err := Accept(context.Background(), db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	now := time.Now().UTC()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-1", "alice", now, DefaultLeaseTTL); err != nil {
		t.Fatalf("acquire lease on populated db: %v", err)
	}
	if got := leaseRowCount(t, db, pid, q.ID); got != 1 {
		t.Errorf("lease rows = %d, want 1", got)
	}
}

// TestAcquireLease_WritesAllColumns verifies a lease row carries session
// id, holder, and the three timestamps with expires_at = acquired_at+TTL.
func TestAcquireLease_WritesAllColumns(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "lease-target"})

	now := fixedNow("2026-06-12T10:00:00Z")()
	const ttl = 90 * time.Second
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-42", "alice", now, ttl); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	l := loadLease(t, db, pid, q.ID)
	if l.SessionID != "sess-42" {
		t.Errorf("session_id = %q, want sess-42", l.SessionID)
	}
	if l.Holder != "alice" {
		t.Errorf("holder = %q, want alice", l.Holder)
	}
	if l.AcquiredAt == "" || l.HeartbeatAt == "" || l.ExpiresAt == "" {
		t.Errorf("timestamps incomplete: %+v", l)
	}
	if l.AcquiredAt != l.HeartbeatAt {
		t.Errorf("acquired_at %q != heartbeat_at %q on fresh acquire", l.AcquiredAt, l.HeartbeatAt)
	}
	wantExp := now.Add(ttl).Format(time.RFC3339Nano)
	if l.ExpiresAt != wantExp {
		t.Errorf("expires_at = %q, want %q (acquired+ttl)", l.ExpiresAt, wantExp)
	}
}

// TestAcquireLease_ReplaceRefreshesSingleRow verifies INSERT OR REPLACE:
// re-acquiring the same quest under a new session refreshes the single
// (project_id, task_id) row instead of duplicating it.
func TestAcquireLease_ReplaceRefreshesSingleRow(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "reacquire"})

	t0 := fixedNow("2026-06-12T10:00:00Z")()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-old", "alice", t0, time.Minute); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t1 := fixedNow("2026-06-12T10:05:00Z")()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-new", "bob", t1, time.Minute); err != nil {
		t.Fatalf("re-acquire: %v", err)
	}

	if got := leaseRowCount(t, db, pid, q.ID); got != 1 {
		t.Fatalf("lease rows after re-acquire = %d, want 1", got)
	}
	l := loadLease(t, db, pid, q.ID)
	if l.SessionID != "sess-new" || l.Holder != "bob" {
		t.Errorf("re-acquire did not refresh: session=%q holder=%q", l.SessionID, l.Holder)
	}
}

// TestRenewLeasesForSession pushes heartbeat_at + expires_at forward for
// every lease a session holds and leaves other sessions untouched.
func TestRenewLeasesForSession(t *testing.T) {
	db, pid := newTestDB(t)
	q1 := mustPost(t, db, pid, PostParams{Subject: "r1"})
	q2 := mustPost(t, db, pid, PostParams{Subject: "r2"})
	other := mustPost(t, db, pid, PostParams{Subject: "other"})

	t0 := fixedNow("2026-06-12T10:00:00Z")()
	for _, q := range []string{q1.ID, q2.ID} {
		if err := AcquireLease(context.Background(), db, pid, q, "sess-A", "alice", t0, time.Minute); err != nil {
			t.Fatalf("acquire %s: %v", q, err)
		}
	}
	if err := AcquireLease(context.Background(), db, pid, other.ID, "sess-B", "bob", t0, time.Minute); err != nil {
		t.Fatalf("acquire other: %v", err)
	}

	t1 := fixedNow("2026-06-12T10:00:30Z")()
	n, err := RenewLeasesForSession(context.Background(), db, "sess-A", t1, time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if n != 2 {
		t.Errorf("renewed = %d, want 2", n)
	}

	wantExp := t1.Add(time.Minute).Format(time.RFC3339Nano)
	for _, q := range []string{q1.ID, q2.ID} {
		l := loadLease(t, db, pid, q)
		if l.HeartbeatAt != t1.Format(time.RFC3339Nano) {
			t.Errorf("%s heartbeat_at = %q, want %q", q, l.HeartbeatAt, t1.Format(time.RFC3339Nano))
		}
		if l.ExpiresAt != wantExp {
			t.Errorf("%s expires_at = %q, want %q", q, l.ExpiresAt, wantExp)
		}
	}
	// sess-B's lease must be untouched.
	lb := loadLease(t, db, pid, other.ID)
	if lb.ExpiresAt != t0.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Errorf("other session lease was modified: expires_at = %q", lb.ExpiresAt)
	}
}

// TestReleaseLeasesForSession deletes every lease a session holds and
// leaves task_status untouched (lease lifecycle is independent of claim).
func TestReleaseLeasesForSession(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "release"})
	if _, err := Accept(context.Background(), db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	now := time.Now().UTC()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-X", "alice", now, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	n, err := ReleaseLeasesForSession(context.Background(), db, "sess-X")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if n != 1 {
		t.Errorf("released = %d, want 1", n)
	}
	if got := leaseRowCount(t, db, pid, q.ID); got != 0 {
		t.Errorf("lease rows after release = %d, want 0", got)
	}
	// The claim itself must survive the lease release.
	if st := mustStatus(t, db, pid, q.ID); st != StatusInProgress {
		t.Errorf("status after lease release = %q, want in_progress", st)
	}
}

// TestExpiredLeases returns only leases whose expires_at <= now, ordered.
func TestExpiredLeases(t *testing.T) {
	db, pid := newTestDB(t)
	expired := mustPost(t, db, pid, PostParams{Subject: "expired"})
	live := mustPost(t, db, pid, PostParams{Subject: "live"})

	base := fixedNow("2026-06-12T10:00:00Z")()
	// expired: acquired at base with a 1s TTL.
	if err := AcquireLease(context.Background(), db, pid, expired.ID, "sess-e", "alice", base, time.Second); err != nil {
		t.Fatalf("acquire expired: %v", err)
	}
	// live: acquired at base with a 1h TTL.
	if err := AcquireLease(context.Background(), db, pid, live.ID, "sess-l", "bob", base, time.Hour); err != nil {
		t.Fatalf("acquire live: %v", err)
	}

	now := fixedNow("2026-06-12T10:01:00Z")() // 60s later
	got, err := ExpiredLeases(context.Background(), db, now)
	if err != nil {
		t.Fatalf("ExpiredLeases: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expired count = %d, want 1: %+v", len(got), got)
	}
	if got[0].TaskID != expired.ID {
		t.Errorf("expired lease = %s, want %s", got[0].TaskID, expired.ID)
	}
	if got[0].SessionID != "sess-e" {
		t.Errorf("expired session = %q, want sess-e", got[0].SessionID)
	}
}

// TestDeleteLease removes a single lease and is a no-op on a missing one.
func TestDeleteLease(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "delete"})
	now := time.Now().UTC()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-d", "alice", now, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := DeleteLease(context.Background(), db, pid, q.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := leaseRowCount(t, db, pid, q.ID); got != 0 {
		t.Errorf("lease rows after delete = %d, want 0", got)
	}
	// Deleting a missing lease is a no-op, not an error.
	if err := DeleteLease(context.Background(), db, pid, "QUEST-9999"); err != nil {
		t.Errorf("delete missing lease errored: %v", err)
	}
}

// TestAcquireLease_Validation rejects empty required fields and a nil DB.
func TestAcquireLease_Validation(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "validate"})
	now := time.Now().UTC()

	if err := AcquireLease(context.Background(), nil, pid, q.ID, "s", "a", now, time.Minute); err == nil {
		t.Error("nil db: want error")
	}
	if err := AcquireLease(context.Background(), db, "", q.ID, "s", "a", now, time.Minute); err == nil {
		t.Error("empty project: want error")
	}
	if err := AcquireLease(context.Background(), db, pid, "", "s", "a", now, time.Minute); err == nil {
		t.Error("empty task: want error")
	}
	if err := AcquireLease(context.Background(), db, pid, q.ID, "", "a", now, time.Minute); err == nil {
		t.Error("empty session: want error")
	}
	// Empty holder defaults to "agent" rather than erroring.
	if err := AcquireLease(context.Background(), db, pid, q.ID, "s", "", now, time.Minute); err != nil {
		t.Errorf("empty holder should default, got error: %v", err)
	}
	if l := loadLease(t, db, pid, q.ID); l.Holder != "agent" {
		t.Errorf("empty holder = %q, want agent", l.Holder)
	}
}

// ---- accept-seam wiring (acceptance: non-nil seam, nil seam, failure) ----

// recordingAcquirer is a test LeaseAcquirer that records its calls and
// optionally writes a real row via an embedded DBLeaseAcquirer.
type recordingAcquirer struct {
	mu     sync.Mutex
	calls  int
	failed bool
	inner  *DBLeaseAcquirer
}

func (r *recordingAcquirer) AcquireLease(ctx context.Context, projectID, taskID, holder string) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if r.failed {
		return fmt.Errorf("injected lease failure")
	}
	if r.inner != nil {
		return r.inner.AcquireLease(ctx, projectID, taskID, holder)
	}
	return nil
}

func (r *recordingAcquirer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// runAcceptHandler executes the quest_accept registry handler with the
// given lease seam, returning the handler error. OpenDB returns a fresh
// *sql.DB per call (against path) because the handler defer-closes it;
// the caller's shared assertion handle stays open.
func runAcceptHandler(t *testing.T, path, pid, questID string, lease any) error {
	t.Helper()
	d := command.Deps{
		OpenDB: func(ctx context.Context) (*sql.DB, error) {
			return storage.Open(ctx, path)
		},
		ResolveProj: func(_ context.Context, _ string) (string, error) { return pid, nil },
		Now:         time.Now,
		Lease:       lease,
	}
	_, err := AcceptCommand.Handler(context.Background(), d, AcceptInput{QuestID: questID})
	return err
}

// TestAcceptSeam_NonNilCreatesExactlyOneLease verifies that with a non-nil
// lease seam, a successful quest_accept creates exactly one task_leases
// row carrying the session id, holder, and the three timestamps.
func TestAcceptSeam_NonNilCreatesExactlyOneLease(t *testing.T) {
	db, pid, path := newLeaseTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "seam-on"})

	acq := &recordingAcquirer{inner: &DBLeaseAcquirer{
		DB:        db,
		SessionID: "sess-seam",
		TTL:       DefaultLeaseTTL,
		Now:       fixedNow("2026-06-12T12:00:00Z"),
	}}
	if err := runAcceptHandler(t, path, pid, q.ID, acq); err != nil {
		t.Fatalf("accept handler: %v", err)
	}

	if acq.count() != 1 {
		t.Errorf("acquirer calls = %d, want 1", acq.count())
	}
	if got := leaseRowCount(t, db, pid, q.ID); got != 1 {
		t.Fatalf("lease rows = %d, want exactly 1", got)
	}
	l := loadLease(t, db, pid, q.ID)
	if l.SessionID != "sess-seam" {
		t.Errorf("session_id = %q, want sess-seam", l.SessionID)
	}
	if l.Holder == "" {
		t.Error("holder empty")
	}
	if l.AcquiredAt == "" || l.HeartbeatAt == "" || l.ExpiresAt == "" {
		t.Errorf("timestamps incomplete: %+v", l)
	}
}

// TestAcceptSeam_NilCreatesNoLease is the no-daemon byte-identical
// regression: with a nil lease seam (the default), quest_accept creates
// zero task_leases rows and its task_status/task_events/task_notes writes
// match a baseline accept that never touched the lease path.
func TestAcceptSeam_NilCreatesNoLease(t *testing.T) {
	// Baseline: accept via the plain Accept function (no handler, no seam).
	baseDB, basePID := newTestDB(t)
	bq := mustPost(t, baseDB, basePID, PostParams{Subject: "baseline"})
	if _, err := Accept(context.Background(), baseDB, basePID, bq.ID, ""); err != nil {
		t.Fatalf("baseline accept: %v", err)
	}

	// Subject: accept via the handler with an explicitly nil lease seam.
	db, pid, path := newLeaseTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "baseline"})
	if err := runAcceptHandler(t, path, pid, q.ID, nil); err != nil {
		t.Fatalf("handler accept: %v", err)
	}

	// Zero lease rows on the nil-seam path.
	if got := leaseRowCount(t, db, pid, q.ID); got != 0 {
		t.Errorf("nil seam created %d lease rows, want 0", got)
	}
	var anyLeases int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_leases`).Scan(&anyLeases); err != nil {
		t.Fatalf("count all leases: %v", err)
	}
	if anyLeases != 0 {
		t.Errorf("nil seam left %d total lease rows, want 0", anyLeases)
	}

	// The trail writes must match the baseline exactly: same status, same
	// event kinds, same note shape. Compare the structural fingerprint of
	// each accept (the timestamps and ids differ, the structure must not).
	if gotStatus, wantStatus := mustStatus(t, db, pid, q.ID), mustStatus(t, baseDB, basePID, bq.ID); gotStatus != wantStatus {
		t.Errorf("status = %q, baseline = %q", gotStatus, wantStatus)
	}
	gotEvents := eventKinds(t, db, pid, q.ID)
	wantEvents := eventKinds(t, baseDB, basePID, bq.ID)
	if fmt.Sprint(gotEvents) != fmt.Sprint(wantEvents) {
		t.Errorf("event kinds = %v, baseline = %v", gotEvents, wantEvents)
	}
	gotNotes := noteShapes(t, db, pid, q.ID)
	wantNotes := noteShapes(t, baseDB, basePID, bq.ID)
	if fmt.Sprint(gotNotes) != fmt.Sprint(wantNotes) {
		t.Errorf("note shapes = %v, baseline = %v", gotNotes, wantNotes)
	}
}

// TestAcceptSeam_FailureNeverFailsAccept verifies a lease-acquire failure
// never converts a committed claim into an error: the claim stands and
// the handler returns success (same invariant as the trail writer).
func TestAcceptSeam_FailureNeverFailsAccept(t *testing.T) {
	db, pid, path := newLeaseTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "seam-fails"})

	acq := &recordingAcquirer{failed: true}
	if err := runAcceptHandler(t, path, pid, q.ID, acq); err != nil {
		t.Fatalf("accept handler returned error despite committed claim: %v", err)
	}
	if acq.count() != 1 {
		t.Errorf("acquirer calls = %d, want 1", acq.count())
	}
	// Claim is durable.
	if st := mustStatus(t, db, pid, q.ID); st != StatusInProgress {
		t.Errorf("status = %q, want in_progress", st)
	}
	// No lease row was written (the failing acquirer wrote nothing).
	if got := leaseRowCount(t, db, pid, q.ID); got != 0 {
		t.Errorf("lease rows = %d, want 0 after failed acquire", got)
	}
}

// TestAcceptSeam_WrongTypeFallsBackToNoLease verifies a non-LeaseAcquirer
// value in Deps.Lease falls back to the no-lease path (nil on mismatch),
// never panicking, so an adapter mis-assignment degrades safely.
func TestAcceptSeam_WrongTypeFallsBackToNoLease(t *testing.T) {
	db, pid, path := newLeaseTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "wrong-type"})

	if err := runAcceptHandler(t, path, pid, q.ID, "not-an-acquirer"); err != nil {
		t.Fatalf("accept handler: %v", err)
	}
	if got := leaseRowCount(t, db, pid, q.ID); got != 0 {
		t.Errorf("wrong-type seam created %d lease rows, want 0", got)
	}
}

// TestAcquireLease_Race is the lease analogue of the accept race: 32
// concurrent accepts on one quest produce exactly one winning claim and,
// because every accept routes through the same daemon-bound acquirer, at
// most one lease row. INSERT OR REPLACE keyed on (project_id, task_id)
// guarantees the single-row invariant even under contention.
func TestAcquireLease_Race(t *testing.T) {
	db, pid, path := newLeaseTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "contested-lease"})

	acq := &recordingAcquirer{inner: &DBLeaseAcquirer{
		DB:        db,
		SessionID: "sess-race",
		TTL:       DefaultLeaseTTL,
	}}

	const N = 32
	var (
		wg     sync.WaitGroup
		start  sync.WaitGroup
		succOk int64
	)
	start.Add(1)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			start.Wait()
			if err := runAcceptHandler(t, path, pid, q.ID, acq); err == nil {
				atomic.AddInt64(&succOk, 1)
			}
		}()
	}
	start.Done()
	wg.Wait()

	if got := atomic.LoadInt64(&succOk); got != 1 {
		t.Errorf("successful accepts = %d, want exactly 1", got)
	}
	// At most one lease row regardless of how many acquirer calls raced.
	if got := leaseRowCount(t, db, pid, q.ID); got > 1 {
		t.Errorf("lease rows = %d, want at most 1", got)
	}
	// Exactly one accept won, so exactly one lease was acquired.
	if got := leaseRowCount(t, db, pid, q.ID); got != 1 {
		t.Errorf("lease rows = %d, want exactly 1 (one winner)", got)
	}
}

// --- local helpers for the byte-identical regression ---

// eventKinds returns the ordered list of event kinds for a quest.
func eventKinds(t *testing.T, db *sql.DB, pid, taskID string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT event FROM task_events WHERE project_id = ? AND task_id = ? ORDER BY id`,
		pid, taskID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// noteShapes returns each note with its trailing variable text stripped to
// the system prefix, so two accepts by different owners compare equal in
// structure (the owner name and timestamps legitimately differ).
func noteShapes(t *testing.T, db *sql.DB, pid, taskID string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT note FROM task_notes WHERE project_id = ? AND task_id = ? ORDER BY id`,
		pid, taskID)
	if err != nil {
		t.Fatalf("query notes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan note: %v", err)
		}
		// Keep the structural prefix only (e.g. "[checkpoint] accepted by").
		shape := n
		if idx := indexAfterPrefix(n); idx > 0 {
			shape = n[:idx]
		}
		out = append(out, shape)
	}
	return out
}

// indexAfterPrefix returns the offset just past the "accepted by " marker
// so noteShapes drops the owner name that legitimately differs.
func indexAfterPrefix(note string) int {
	const marker = "accepted by "
	if i := indexOf(note, marker); i >= 0 {
		return i + len(marker)
	}
	return 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
