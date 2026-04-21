package quest

import (
	"context"
	"errors"
	"testing"
)

// TestForfeit_InProgress_ReturnsToNext verifies the happy path:
// forfeit on status=in_progress releases the claim and flips to next.
func TestForfeit_InProgress_ReturnsToNext(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "cycle"})

	if _, err := Accept(context.Background(), db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if s := mustStatus(t, db, pid, q.ID); s != StatusInProgress {
		t.Fatalf("pre-forfeit status = %s", s)
	}

	got, err := Forfeit(context.Background(), db, pid, q.ID, "blocked on spec")
	if err != nil {
		t.Fatalf("Forfeit: %v", err)
	}
	if got.AlreadyNext {
		t.Errorf("AlreadyNext = true, want false for in_progress input")
	}
	if got.Quest.Status != StatusNext {
		t.Errorf("status = %q, want next", got.Quest.Status)
	}
	if got.Quest.Owner != "" {
		t.Errorf("owner = %q, want empty after forfeit", got.Quest.Owner)
	}

	// released event landed.
	var events int
	err = db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_events WHERE project_id = ? AND task_id = ? AND event = 'released'`,
		pid, q.ID,
	).Scan(&events)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("released events = %d, want 1", events)
	}
	// [released] note landed.
	var notes int
	err = db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_notes WHERE project_id = ? AND task_id = ? AND note LIKE '[released]%'`,
		pid, q.ID,
	).Scan(&notes)
	if err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if notes != 1 {
		t.Errorf("[released] notes = %d, want 1", notes)
	}
}

func TestForfeit_InProgress_NoNote(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s"})
	_, _ = Accept(context.Background(), db, pid, q.ID, "a")
	if _, err := Forfeit(context.Background(), db, pid, q.ID, ""); err != nil {
		t.Fatalf("Forfeit: %v", err)
	}
	// No [released] note should exist.
	var notes int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM task_notes WHERE project_id = ? AND task_id = ? AND note LIKE '[released]%'`,
		pid, q.ID,
	).Scan(&notes)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if notes != 0 {
		t.Errorf("note count = %d, want 0 (empty note)", notes)
	}
}

// TestForfeit_Done_RefusesWithError is the QUEST-135 regression:
// forfeit on a done quest must error out with ErrAlreadyDone and
// leave the status untouched. No released event, no [released] note.
func TestForfeit_Done_RefusesWithError(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	q := mustPost(t, db, pid, PostParams{Subject: "finished work"})

	if _, err := Accept(ctx, db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := Clear(ctx, db, pid, q.ID, "done"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if s := mustStatus(t, db, pid, q.ID); s != StatusDone {
		t.Fatalf("pre-forfeit status = %s, want done", s)
	}

	got, err := Forfeit(ctx, db, pid, q.ID, "oops")
	if err == nil {
		t.Fatalf("Forfeit on done: want error, got result=%+v", got)
	}
	if !errors.Is(err, ErrAlreadyDone) {
		t.Errorf("err = %v, want ErrAlreadyDone", err)
	}

	// Status must remain done — forfeit must not silently reopen.
	if s := mustStatus(t, db, pid, q.ID); s != StatusDone {
		t.Errorf("post-forfeit status = %s, want done (forfeit must not reopen)", s)
	}

	// No released event should have been written.
	var events int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_events WHERE project_id = ? AND task_id = ? AND event = 'released'`,
		pid, q.ID,
	).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("released events = %d, want 0 (done quest must not emit release event)", events)
	}

	// No [released] note either.
	var notes int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_notes WHERE project_id = ? AND task_id = ? AND note LIKE '[released]%'`,
		pid, q.ID,
	).Scan(&notes); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if notes != 0 {
		t.Errorf("[released] notes = %d, want 0", notes)
	}
}

// TestForfeit_Next_IsNoOp is the QUEST-135 regression: forfeit on a
// status=next quest is a neutral no-op — AlreadyNext=true in the
// result, no released event, and status stays 'next'.
func TestForfeit_Next_IsNoOp(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	q := mustPost(t, db, pid, PostParams{Subject: "unclaimed"})
	if s := mustStatus(t, db, pid, q.ID); s != StatusNext {
		t.Fatalf("pre-forfeit status = %s, want next", s)
	}

	got, err := Forfeit(ctx, db, pid, q.ID, "ignored note")
	if err != nil {
		t.Fatalf("Forfeit on next: %v", err)
	}
	if got == nil || !got.AlreadyNext {
		t.Fatalf("AlreadyNext = false, want true for next input")
	}
	if got.Quest == nil || got.Quest.Status != StatusNext {
		t.Errorf("quest.Status = %q, want next", got.Quest.Status)
	}

	// No released event should land when the quest wasn't in_progress.
	var events int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_events WHERE project_id = ? AND task_id = ? AND event = 'released'`,
		pid, q.ID,
	).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("released events = %d, want 0 (next no-op must not emit release event)", events)
	}

	// No [released] note should be persisted even when a note was passed.
	var notes int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_notes WHERE project_id = ? AND task_id = ? AND note LIKE '[released]%'`,
		pid, q.ID,
	).Scan(&notes); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if notes != 0 {
		t.Errorf("[released] notes = %d, want 0 for no-op forfeit", notes)
	}
}

func TestForfeit_NotFound(t *testing.T) {
	db, pid := newTestDB(t)
	_, err := Forfeit(context.Background(), db, pid, "QUEST-404", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestForfeit_AcceptCycle verifies a post→accept→forfeit→accept round-trip.
func TestForfeit_AcceptCycle(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	q := mustPost(t, db, pid, PostParams{Subject: "cycle"})

	if _, err := Accept(ctx, db, pid, q.ID, "alice"); err != nil {
		t.Fatalf("accept1: %v", err)
	}
	if _, err := Forfeit(ctx, db, pid, q.ID, ""); err != nil {
		t.Fatalf("forfeit: %v", err)
	}
	// Second accept must succeed.
	got, err := Accept(ctx, db, pid, q.ID, "bob")
	if err != nil {
		t.Fatalf("accept2: %v", err)
	}
	if got.Owner != "bob" {
		t.Errorf("owner after re-accept = %q, want bob", got.Owner)
	}
}
