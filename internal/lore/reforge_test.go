package lore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestReforge_HappyPath marks oldID superseded + writes the supersedes
// edge in one transaction.
func TestReforge_HappyPath(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")
	oldE, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindDecision,
		Title:   "old decision to be reforged into a new one",
		Summary: "s", Topic: "x",
	})
	newE, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindDecision,
		Title:   "new decision replacing the old one explicitly",
		Summary: "s", Topic: "x",
	})

	if err := Reforge(ctx, db, oldE.Entry.ID, newE.Entry.ID, time.Time{}, nil); err != nil {
		t.Fatalf("reforge: %v", err)
	}

	// Verify status + edge both landed.
	var status string
	_ = db.QueryRowContext(ctx, `SELECT status FROM entries WHERE id = ?`,
		oldE.Entry.ID).Scan(&status)
	if status != string(StatusSuperseded) {
		t.Errorf("want status=superseded, got %q", status)
	}
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entry_links
		 WHERE from_id = ? AND to_id = ? AND relation = 'supersedes'`,
		newE.Entry.ID, oldE.Entry.ID,
	).Scan(&n)
	if n != 1 {
		t.Errorf("want 1 supersedes edge, got %d", n)
	}
}

// TestReforge_AtomicityOnFault is the quest-critical test: inject a
// failure BETWEEN the UPDATE status and the INSERT link, and prove that
// neither change survives. This is the non-negotiable integration gate.
func TestReforge_AtomicityOnFault(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")
	oldE, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindDecision,
		Title:   "old decision to be reforged into a new one atomic",
		Summary: "s", Topic: "x",
	})
	newE, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindDecision,
		Title:   "new decision replacing the old one atomic verify",
		Summary: "s", Topic: "x",
	})

	// Snapshot pre-state so we can compare after the rollback.
	preUpdated := getUpdatedAt(t, db, oldE.Entry.ID)

	// Install the test fault seam: force a failure between UPDATE and
	// INSERT. The returned error is what Reforge bubbles up.
	injected := errors.New("simulated mid-transaction failure")
	reforgeFaultForTest = func() error { return injected }
	t.Cleanup(func() { reforgeFaultForTest = nil })

	err := Reforge(ctx, db, oldE.Entry.ID, newE.Entry.ID, time.Time{}, nil)
	if err == nil {
		t.Fatalf("expected injected error, got nil")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("want error chain to contain injected %v, got %v", injected, err)
	}
	t.Logf("reforge errored as expected: %v", err)

	// Verify status is STILL 'current' — the UPDATE inside the tx
	// must have been rolled back.
	var status string
	_ = db.QueryRowContext(ctx, `SELECT status FROM entries WHERE id = ?`,
		oldE.Entry.ID).Scan(&status)
	if status != string(StatusCurrent) {
		t.Errorf("status leaked after rollback: got %q, want %q", status, StatusCurrent)
	}
	// updated_at should also be unchanged (the tx's UPDATE was rolled
	// back so it never persisted — any datetime bump is evidence of
	// a leaked write).
	postUpdated := getUpdatedAt(t, db, oldE.Entry.ID)
	if postUpdated != preUpdated {
		t.Errorf("updated_at changed across rollback: pre=%q post=%q",
			preUpdated, postUpdated)
	}

	// Verify NO entry_links row was created (the INSERT never happened
	// AND any partial state was rolled back).
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entry_links`).Scan(&n)
	if n != 0 {
		t.Errorf("entry_links row leaked after rollback: got %d, want 0", n)
	}

	t.Logf("post-rollback invariants held: status=%s, link_rows=%d", status, n)
}

// TestReforge_SelfIsError catches the obvious CLI typo.
func TestReforge_SelfIsError(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")
	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindDecision,
		Title:   "entry that would reforge into itself test demo case",
		Summary: "s", Topic: "x",
	})
	err := Reforge(ctx, db, a.Entry.ID, a.Entry.ID, time.Time{}, nil)
	if !errors.Is(err, ErrReforgeSelf) {
		t.Errorf("want ErrReforgeSelf, got %v", err)
	}
}

// TestReforge_MissingIDsErrorBeforeTx rejects unknown ids cleanly.
func TestReforge_MissingIDsErrorBeforeTx(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")
	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindDecision,
		Title:   "entry for missing id rejection test case thing",
		Summary: "s", Topic: "x",
	})
	err := Reforge(ctx, db, a.Entry.ID, 9999, time.Time{}, nil)
	if !errors.Is(err, ErrEntryNotFound) {
		t.Errorf("want ErrEntryNotFound, got %v", err)
	}
}

// getUpdatedAt reads entries.updated_at as a raw string so the
// atomicity test can compare exact equality across the injected-
// failure rollback.
func getUpdatedAt(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var s string
	if err := db.QueryRowContext(context.Background(),
		`SELECT updated_at FROM entries WHERE id = ?`, id,
	).Scan(&s); err != nil {
		t.Fatalf("getUpdatedAt %d: %v", id, err)
	}
	return s
}
