package quest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/storage"
)

// TestRenewLeaseForSessionTask_RenewsOnlyHeldRow verifies the targeted
// activity-renewal primitive pushes heartbeat_at/expires_at forward for
// the matched (session, project, task) and renews nothing else.
func TestRenewLeaseForSessionTask_RenewsOnlyHeldRow(t *testing.T) {
	db, pid := newTestDB(t)
	mine := mustPost(t, db, pid, PostParams{Subject: "mine"})
	theirs := mustPost(t, db, pid, PostParams{Subject: "theirs"})

	t0 := fixedNow("2026-06-12T10:00:00Z")()
	if err := AcquireLease(context.Background(), db, pid, mine.ID, "sess-A", "alice", t0, time.Minute); err != nil {
		t.Fatalf("acquire mine: %v", err)
	}
	if err := AcquireLease(context.Background(), db, pid, theirs.ID, "sess-B", "bob", t0, time.Minute); err != nil {
		t.Fatalf("acquire theirs: %v", err)
	}

	t1 := fixedNow("2026-06-12T10:00:30Z")()
	n, err := RenewLeaseForSessionTask(context.Background(), db, "sess-A", pid, mine.ID, t1, time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows renewed = %d, want 1", n)
	}

	got := loadLease(t, db, pid, mine.ID)
	wantExp := t1.Add(time.Minute).Format(time.RFC3339Nano)
	if got.ExpiresAt != wantExp {
		t.Errorf("mine expires_at = %q, want %q", got.ExpiresAt, wantExp)
	}
	if got.HeartbeatAt != t1.Format(time.RFC3339Nano) {
		t.Errorf("mine heartbeat_at = %q, want %q", got.HeartbeatAt, t1.Format(time.RFC3339Nano))
	}

	// The other session's lease is untouched.
	other := loadLease(t, db, pid, theirs.ID)
	if other.ExpiresAt != t0.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Errorf("theirs lease was renewed: expires_at = %q", other.ExpiresAt)
	}
}

// TestRenewLeaseForSessionTask_WrongSessionNoOp verifies a renewal scoped
// to the wrong session renews zero rows (a write to a quest some other
// session leased never refreshes that other session's lease).
func TestRenewLeaseForSessionTask_WrongSessionNoOp(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "held"})
	t0 := fixedNow("2026-06-12T10:00:00Z")()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-A", "alice", t0, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	n, err := RenewLeaseForSessionTask(context.Background(), db, "sess-OTHER", pid, q.ID,
		fixedNow("2026-06-12T10:00:30Z")(), time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows renewed = %d, want 0 (wrong session)", n)
	}
	// Unchanged.
	got := loadLease(t, db, pid, q.ID)
	if got.ExpiresAt != t0.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Errorf("lease was renewed by wrong session: %q", got.ExpiresAt)
	}
}

// TestRenewAllLeases_BootGrace verifies the boot-grace primitive pushes
// every lease forward regardless of session, and returns the count.
func TestRenewAllLeases_BootGrace(t *testing.T) {
	db, pid := newTestDB(t)
	q1 := mustPost(t, db, pid, PostParams{Subject: "a"})
	q2 := mustPost(t, db, pid, PostParams{Subject: "b"})

	t0 := fixedNow("2026-06-12T10:00:00Z")()
	if err := AcquireLease(context.Background(), db, pid, q1.ID, "sess-A", "alice", t0, time.Minute); err != nil {
		t.Fatalf("acquire q1: %v", err)
	}
	if err := AcquireLease(context.Background(), db, pid, q2.ID, "sess-B", "bob", t0, time.Minute); err != nil {
		t.Fatalf("acquire q2: %v", err)
	}

	boot := fixedNow("2026-06-12T10:05:00Z")()
	const ttl = 10 * time.Minute
	n, err := RenewAllLeases(context.Background(), db, boot, ttl)
	if err != nil {
		t.Fatalf("renew all: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows renewed = %d, want 2", n)
	}
	wantExp := boot.Add(ttl).Format(time.RFC3339Nano)
	for _, q := range []string{q1.ID, q2.ID} {
		got := loadLease(t, db, pid, q)
		if got.ExpiresAt != wantExp {
			t.Errorf("%s expires_at = %q, want %q (boot grace)", q, got.ExpiresAt, wantExp)
		}
		if got.HeartbeatAt != boot.Format(time.RFC3339Nano) {
			t.Errorf("%s heartbeat_at = %q, want %q", q, got.HeartbeatAt, boot.Format(time.RFC3339Nano))
		}
	}
}

// TestRenewAllLeases_EmptyTableNoError verifies boot grace on a quest.db
// that never took a lease is a clean no-op (zero rows, no error).
func TestRenewAllLeases_EmptyTableNoError(t *testing.T) {
	db, _ := newTestDB(t)
	n, err := RenewAllLeases(context.Background(), db, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatalf("renew all on empty table: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows renewed = %d, want 0", n)
	}
}

// TestDBLeaseAcquirer_RenewLeaseActivity verifies the DBLeaseAcquirer's
// activity-renewal method refreshes the session's own held lease.
func TestDBLeaseAcquirer_RenewLeaseActivity(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "activity"})

	t0 := fixedNow("2026-06-12T10:00:00Z")()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-X", "alice", t0, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	t1 := fixedNow("2026-06-12T10:00:45Z")()
	acq := &DBLeaseAcquirer{DB: db, SessionID: "sess-X", TTL: time.Minute, Now: func() time.Time { return t1 }}
	if err := acq.RenewLeaseActivity(context.Background(), pid, q.ID); err != nil {
		t.Fatalf("renew activity: %v", err)
	}

	got := loadLease(t, db, pid, q.ID)
	if got.ExpiresAt != t1.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Errorf("expires_at = %q, want %q (activity-renewed)", got.ExpiresAt, t1.Add(time.Minute).Format(time.RFC3339Nano))
	}
}

// TestActivityRenewal_ThroughJournalHandler verifies the end-to-end
// activity-renewal seam: a daemon-mediated quest_journal on a leased quest
// refreshes that session's lease. The seam is the same `any`-typed
// Deps.Lease the accept handler uses, satisfying both LeaseAcquirer and
// LeaseRenewer.
func TestActivityRenewal_ThroughJournalHandler(t *testing.T) {
	db, pid, path := newLeaseTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "journal-renew"})
	if _, err := Accept(context.Background(), db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	t0 := fixedNow("2026-06-12T10:00:00Z")()
	if err := AcquireLease(context.Background(), db, pid, q.ID, "sess-J", "alice", t0, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	t1 := fixedNow("2026-06-12T10:00:50Z")()
	acq := &DBLeaseAcquirer{DB: db, SessionID: "sess-J", TTL: time.Minute, Now: func() time.Time { return t1 }}
	d := command.Deps{
		OpenDB:      func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, path) },
		ResolveProj: func(_ context.Context, _ string) (string, error) { return pid, nil },
		Now:         time.Now,
		Lease:       acq,
	}
	if _, err := JournalCommand.Handler(context.Background(), d, JournalInput{QuestID: q.ID, Text: "progress"}); err != nil {
		t.Fatalf("journal handler: %v", err)
	}

	got := loadLease(t, db, pid, q.ID)
	wantExp := t1.Add(time.Minute).Format(time.RFC3339Nano)
	if got.ExpiresAt != wantExp {
		t.Errorf("expires_at = %q, want %q (renewed by journal activity)", got.ExpiresAt, wantExp)
	}
}

// TestActivityRenewal_NilSeamNoLeaseRows is the no-daemon byte-identical
// regression for the mutating path: a quest_journal with a nil lease seam
// (the default) creates zero task_leases rows and renews nothing, exactly
// as today.
func TestActivityRenewal_NilSeamNoLeaseRows(t *testing.T) {
	db, pid, path := newLeaseTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "no-daemon"})
	if _, err := Accept(context.Background(), db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	d := command.Deps{
		OpenDB:      func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, path) },
		ResolveProj: func(_ context.Context, _ string) (string, error) { return pid, nil },
		Now:         time.Now,
		Lease:       nil, // no daemon
	}
	if _, err := JournalCommand.Handler(context.Background(), d, JournalInput{QuestID: q.ID, Text: "progress"}); err != nil {
		t.Fatalf("journal handler: %v", err)
	}

	var anyLeases int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_leases`).Scan(&anyLeases); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if anyLeases != 0 {
		t.Errorf("nil seam created %d lease rows on journal, want 0", anyLeases)
	}
}
