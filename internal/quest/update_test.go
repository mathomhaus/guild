package quest

import (
	"context"
	"errors"
	"testing"
)

func TestUpdate_ScalarOverwrite(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s", Priority: "P2"})
	if _, err := Update(context.Background(), db, pid, q.ID, UpdateParams{Priority: "P0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := mustLoad(t, db, pid, q.ID)
	if got.Priority != "P0" {
		t.Errorf("priority = %q, want P0 (not appended)", got.Priority)
	}
	// Subject unchanged.
	if got.Subject != "s" {
		t.Errorf("subject = %q, want s", got.Subject)
	}
}

func TestUpdate_ScalarMultipleOverwrites(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s", Priority: "P2"})
	ctx := context.Background()
	if _, err := Update(ctx, db, pid, q.ID, UpdateParams{Priority: "P1"}); err != nil {
		t.Fatalf("1: %v", err)
	}
	if _, err := Update(ctx, db, pid, q.ID, UpdateParams{Priority: "P0"}); err != nil {
		t.Fatalf("2: %v", err)
	}
	got := mustLoad(t, db, pid, q.ID)
	if got.Priority != "P0" {
		t.Errorf("priority = %q, want P0 (last wins)", got.Priority)
	}
}

func TestUpdate_ListAppend_Files(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s", Files: []string{"a.go"}})
	if _, err := Update(context.Background(), db, pid, q.ID, UpdateParams{Files: []string{"b.go"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := mustLoad(t, db, pid, q.ID)
	if len(got.Files) != 2 || got.Files[0] != "a.go" || got.Files[1] != "b.go" {
		t.Errorf("files = %v, want [a.go b.go]", got.Files)
	}
}

func TestUpdate_ListReplace_Files(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s", Files: []string{"a.go"}})
	if _, err := Update(context.Background(), db, pid, q.ID, UpdateParams{
		ReplaceFiles: []string{"b.go"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := mustLoad(t, db, pid, q.ID)
	if len(got.Files) != 1 || got.Files[0] != "b.go" {
		t.Errorf("files = %v, want [b.go]", got.Files)
	}
}

func TestUpdate_ListReplace_Acceptance(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{
		Subject:    "s",
		Acceptance: []string{"old1", "old2"},
	})
	if _, err := Update(context.Background(), db, pid, q.ID, UpdateParams{
		ReplaceAcceptance: []string{"new1", "new2", "new3"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := mustLoad(t, db, pid, q.ID)
	if len(got.Acceptance) != 3 {
		t.Fatalf("acceptance = %v, want 3", got.Acceptance)
	}
	for i, want := range []string{"new1", "new2", "new3"} {
		if got.Acceptance[i] != want {
			t.Errorf("acc[%d] = %q, want %q", i, got.Acceptance[i], want)
		}
	}
}

func TestUpdate_ListAppend_Acceptance_PreservesCommas(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s"})
	if _, err := Update(context.Background(), db, pid, q.ID, UpdateParams{
		Acceptance: []string{"foo, bar, baz", "one; two"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := mustLoad(t, db, pid, q.ID)
	if len(got.Acceptance) != 2 {
		t.Fatalf("acceptance = %v, want 2", got.Acceptance)
	}
	if got.Acceptance[0] != "foo, bar, baz" {
		t.Errorf("acc[0] = %q", got.Acceptance[0])
	}
}

func TestUpdate_ConflictingAppendAndReplace(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s"})
	_, err := Update(context.Background(), db, pid, q.ID, UpdateParams{
		Files:        []string{"a.go"},
		ReplaceFiles: []string{"b.go"},
	})
	if !errors.Is(err, ErrConflictingUpdate) {
		t.Errorf("err = %v, want ErrConflictingUpdate", err)
	}
}

func TestUpdate_NoChange(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "s"})
	_, err := Update(context.Background(), db, pid, q.ID, UpdateParams{})
	if !errors.Is(err, ErrNoChange) {
		t.Errorf("err = %v, want ErrNoChange", err)
	}
}

func TestUpdate_AutoBlockOnNewDep(t *testing.T) {
	db, pid := newTestDB(t)
	a := mustPost(t, db, pid, PostParams{Subject: "A"}) // next
	b := mustPost(t, db, pid, PostParams{Subject: "B"}) // next, no deps

	// Add a dep → B must flip to blocked.
	if _, err := Update(context.Background(), db, pid, b.ID, UpdateParams{
		DependsOn: []string{a.ID},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusBlocked {
		t.Errorf("B status = %s, want blocked after adding dep on A (not done)", s)
	}
	// Now clear A — B must unblock.
	res, err := Clear(context.Background(), db, pid, a.ID, "")
	if err != nil {
		t.Fatalf("Clear A: %v", err)
	}
	if len(res.Unblocked) != 1 || res.Unblocked[0].ID != b.ID {
		t.Errorf("unblocked = %v, want [%s]", idsOf(res.Unblocked), b.ID)
	}
}

// TestUpdate_AutoUnblock_FulfillDepPath is the QUEST-147 fulfill-dep
// regression: B is blocked via Update-added dep on A; when A is
// fulfilled the cascade (not the Update recompute path) flips B.
// This path was already covered by TestUpdate_AutoBlockOnNewDep; the
// explicit assertion here guards the post-fix blocked→next transition
// path reported by the `Update` recompute helper stays compatible with
// the cascade helper (both use EventUnblocked).
func TestUpdate_AutoUnblock_FulfillDepPath(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	a := mustPost(t, db, pid, PostParams{Subject: "A"})
	b := mustPost(t, db, pid, PostParams{Subject: "B"})

	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{DependsOn: []string{a.ID}}); err != nil {
		t.Fatalf("Update add dep: %v", err)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusBlocked {
		t.Fatalf("B status = %s, want blocked before A fulfilled", s)
	}

	res, err := Fulfill(ctx, db, pid, a.ID, "done-reason")
	if err != nil {
		t.Fatalf("Fulfill A: %v", err)
	}
	if len(res.Unblocked) != 1 || res.Unblocked[0].ID != b.ID {
		t.Fatalf("Fulfill unblocked = %v, want [%s]", idsOf(res.Unblocked), b.ID)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusNext {
		t.Errorf("B status = %s, want next after A fulfilled", s)
	}
}

// TestUpdate_AutoUnblock_ClearDependsOnPath is the QUEST-147 core
// regression: an agent posts B depending on A, realizes the dep was
// wrong, and clears it. Before the fix, B stayed 'blocked' even with
// zero deps on the canonical list — the agent's only recourse was
// raw-SQL surgery, violating the no-SQL-bypass oath.
func TestUpdate_AutoUnblock_ClearDependsOnPath(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	a := mustPost(t, db, pid, PostParams{Subject: "A"})
	b := mustPost(t, db, pid, PostParams{Subject: "B"})

	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{DependsOn: []string{a.ID}}); err != nil {
		t.Fatalf("Update add dep: %v", err)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusBlocked {
		t.Fatalf("B status = %s, want blocked before clear", s)
	}

	// Clear B's deps while A is still not done — B must still unblock
	// because the canonical dep list is now empty.
	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{ClearDependsOn: true}); err != nil {
		t.Fatalf("Update clear deps: %v", err)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusNext {
		t.Errorf("B status = %s, want next after clearing deps (was blocked, now 0 deps)", s)
	}
	reloaded := mustLoad(t, db, pid, b.ID)
	if len(reloaded.DependsOn) != 0 {
		t.Errorf("B deps = %v, want empty after ClearDependsOn", reloaded.DependsOn)
	}
}

// TestUpdate_AutoUnblock_ReplaceDependsOnPath exercises the replace
// form of the same fix: swapping a not-done dep set for a done-or-empty
// dep set must flip blocked→next. Uses a 3-quest graph so the test also
// asserts that cascade-through-multiple-deps is honored — when the new
// dep set contains a not-done entry, B stays blocked.
func TestUpdate_AutoUnblock_ReplaceDependsOnPath(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	a := mustPost(t, db, pid, PostParams{Subject: "A"})
	c := mustPost(t, db, pid, PostParams{Subject: "C"})
	d := mustPost(t, db, pid, PostParams{Subject: "D"})
	b := mustPost(t, db, pid, PostParams{Subject: "B"})

	// B blocked on A.
	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{DependsOn: []string{a.ID}}); err != nil {
		t.Fatalf("Update add dep A: %v", err)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusBlocked {
		t.Fatalf("B status = %s, want blocked", s)
	}

	// Fulfill C (done) but not D.
	if _, err := Fulfill(ctx, db, pid, c.ID, ""); err != nil {
		t.Fatalf("Fulfill C: %v", err)
	}

	// Replace deps with [C, D] — D is not done, so B must STAY blocked.
	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{
		ReplaceDependsOn: []string{c.ID, d.ID},
	}); err != nil {
		t.Fatalf("Update replace deps [C,D]: %v", err)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusBlocked {
		t.Errorf("B status = %s, want blocked (D not done)", s)
	}

	// Replace deps with just [C] — C is done, so B must unblock.
	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{
		ReplaceDependsOn: []string{c.ID},
	}); err != nil {
		t.Fatalf("Update replace deps [C]: %v", err)
	}
	if s := mustStatus(t, db, pid, b.ID); s != StatusNext {
		t.Errorf("B status = %s, want next (replaced deps all done)", s)
	}
	reloaded := mustLoad(t, db, pid, b.ID)
	if len(reloaded.DependsOn) != 1 || reloaded.DependsOn[0] != c.ID {
		t.Errorf("B deps = %v, want [%s]", reloaded.DependsOn, c.ID)
	}
}

func TestUpdate_ClearAcceptance(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{
		Subject:    "s",
		Acceptance: []string{"a", "b"},
	})
	if _, err := Update(context.Background(), db, pid, q.ID, UpdateParams{
		ClearAcceptance: true,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := mustLoad(t, db, pid, q.ID)
	if len(got.Acceptance) != 0 {
		t.Errorf("acceptance = %v, want empty", got.Acceptance)
	}
}

// TestUpdate_ClearDependsOn_BeforeTwoAfterZero is the explicit acceptance-criterion
// shape for issue #47: a quest with two deps, --clear-depends-on drops both.
func TestUpdate_ClearDependsOn_BeforeTwoAfterZero(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	a := mustPost(t, db, pid, PostParams{Subject: "A"})
	c := mustPost(t, db, pid, PostParams{Subject: "C"})
	b := mustPost(t, db, pid, PostParams{Subject: "B", DependsOn: []string{a.ID, c.ID}})

	before := mustLoad(t, db, pid, b.ID)
	if len(before.DependsOn) != 2 {
		t.Fatalf("B before clear: deps = %v, want 2", before.DependsOn)
	}

	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{ClearDependsOn: true}); err != nil {
		t.Fatalf("Update ClearDependsOn: %v", err)
	}

	after := mustLoad(t, db, pid, b.ID)
	if len(after.DependsOn) != 0 {
		t.Errorf("B after clear: deps = %v, want empty", after.DependsOn)
	}
}

// TestUpdate_EmptyDependsOn_RedirectsToClear covers issue #47 acceptance criterion #3:
// `--depends-on ""` (empty/whitespace-only entries) without --clear-depends-on or
// --replace-depends-on must error pointing at --clear-depends-on rather than
// silently no-op'ing.
func TestUpdate_EmptyDependsOn_RedirectsToClear(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	a := mustPost(t, db, pid, PostParams{Subject: "A"})
	b := mustPost(t, db, pid, PostParams{Subject: "B", DependsOn: []string{a.ID}})

	cases := []struct {
		name string
		deps []string
	}{
		{"single empty", []string{""}},
		{"single whitespace", []string{"   "}},
		{"multiple empty", []string{"", "  ", "\t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Update(ctx, db, pid, b.ID, UpdateParams{DependsOn: tc.deps})
			if !errors.Is(err, ErrEmptyDependsOn) {
				t.Fatalf("Update with deps=%v: err = %v, want ErrEmptyDependsOn", tc.deps, err)
			}
			// State must be untouched on error.
			reloaded := mustLoad(t, db, pid, b.ID)
			if len(reloaded.DependsOn) != 1 || reloaded.DependsOn[0] != a.ID {
				t.Errorf("B deps after refused empty update = %v, want [%s]", reloaded.DependsOn, a.ID)
			}
		})
	}

	// Sanity: a mixed slice (real ID + empty) is NOT redirected — the trim/skip
	// path still applies for that case so existing callers aren't broken.
	if _, err := Update(ctx, db, pid, b.ID, UpdateParams{DependsOn: []string{a.ID, ""}}); err != nil {
		t.Fatalf("Update with mixed deps: unexpected error %v", err)
	}
}
