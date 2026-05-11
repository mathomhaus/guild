package quest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestPost_MonotonicID(t *testing.T) {
	db, pid := newTestDB(t)
	q1 := mustPost(t, db, pid, PostParams{Subject: "first"})
	q2 := mustPost(t, db, pid, PostParams{Subject: "second"})
	q3 := mustPost(t, db, pid, PostParams{Subject: "third"})
	if q1.ID != "QUEST-1" {
		t.Errorf("q1.ID = %q, want QUEST-1", q1.ID)
	}
	if q2.ID != "QUEST-2" {
		t.Errorf("q2.ID = %q, want QUEST-2", q2.ID)
	}
	if q3.ID != "QUEST-3" {
		t.Errorf("q3.ID = %q, want QUEST-3", q3.ID)
	}
}

func TestPost_ConcurrentWritersAllocateUniqueIDs(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	const writers = 32

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	ids := make(chan string, writers)
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := Post(ctx, db, pid, PostParams{Subject: fmt.Sprintf("concurrent-%02d", i)})
			if err != nil {
				errs <- err
				return
			}
			ids <- q.ID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)

	for err := range errs {
		t.Errorf("Post returned error under contention: %v", err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id allocated: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != writers {
		t.Fatalf("allocated %d IDs, want %d", len(seen), writers)
	}
	for i := 1; i <= writers; i++ {
		id := fmt.Sprintf("QUEST-%d", i)
		if !seen[id] {
			t.Fatalf("missing allocated id %s; got %v", id, seen)
		}
	}
}

func TestPost_RoundTrip(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{
		Subject:    "lifecycle round-trip",
		Priority:   "P0",
		Epic:       "epic-v1",
		Effort:     "L",
		Files:      []string{"internal/quest/post.go", "internal/quest/clear.go"},
		Acceptance: []string{"cascade chain works", "race-safe accept", "trifecta green"},
		DependsOn:  []string{"QUEST-2", "QUEST-3"},
		ReworkOf:   "QUEST-0",
	})
	// Reload and confirm every field survives.
	reloaded := mustLoad(t, db, pid, q.ID)
	if reloaded.Subject != "lifecycle round-trip" {
		t.Errorf("subject = %q", reloaded.Subject)
	}
	if reloaded.Priority != "P0" {
		t.Errorf("priority = %q", reloaded.Priority)
	}
	if reloaded.Epic != "epic-v1" {
		t.Errorf("epic = %q", reloaded.Epic)
	}
	if reloaded.Effort != "L" {
		t.Errorf("effort = %q", reloaded.Effort)
	}
	if len(reloaded.Files) != 2 || reloaded.Files[0] != "internal/quest/post.go" {
		t.Errorf("files = %v", reloaded.Files)
	}
	if len(reloaded.Acceptance) != 3 || reloaded.Acceptance[1] != "race-safe accept" {
		t.Errorf("acceptance = %v", reloaded.Acceptance)
	}
	if len(reloaded.DependsOn) != 2 || reloaded.DependsOn[0] != "QUEST-2" {
		t.Errorf("depends_on = %v", reloaded.DependsOn)
	}
	if reloaded.ReworkOf != "QUEST-0" {
		t.Errorf("rework_of = %q", reloaded.ReworkOf)
	}
	// With 2 non-done deps, status should be blocked.
	if reloaded.Status != StatusBlocked {
		t.Errorf("status = %q, want blocked (deps not met)", reloaded.Status)
	}
}

func TestPost_NoDepsStaysNext(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{Subject: "standalone"})
	if q.Status != StatusNext {
		t.Errorf("status = %q, want next", q.Status)
	}
}

func TestPost_DepsAllDoneStartsNext(t *testing.T) {
	db, pid := newTestDB(t)
	qA := mustPost(t, db, pid, PostParams{Subject: "A"})
	// Mark A done directly.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE task_status SET status = 'done' WHERE project_id = ? AND task_id = ?`,
		pid, qA.ID,
	); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	qB := mustPost(t, db, pid, PostParams{
		Subject:   "B",
		DependsOn: []string{qA.ID},
	})
	if qB.Status != StatusNext {
		t.Errorf("B status = %q, want next (dep already done)", qB.Status)
	}
}

func TestPost_EmptySubjectRejected(t *testing.T) {
	db, pid := newTestDB(t)
	_, err := Post(context.Background(), db, pid, PostParams{Subject: ""})
	if err == nil {
		t.Fatal("expected error for empty subject")
	}
}

func TestPost_AcceptancePreservesCommas(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{
		Subject: "comma-preservation",
		Acceptance: []string{
			"SELECT a, b, c FROM t works",
			"handles commas, quotes, and semicolons: yes",
		},
	})
	r := mustLoad(t, db, pid, q.ID)
	if len(r.Acceptance) != 2 {
		t.Fatalf("acceptance len = %d (%v), want 2", len(r.Acceptance), r.Acceptance)
	}
	if r.Acceptance[0] != "SELECT a, b, c FROM t works" {
		t.Errorf("crit[0] = %q", r.Acceptance[0])
	}
	if r.Acceptance[1] != "handles commas, quotes, and semicolons: yes" {
		t.Errorf("crit[1] = %q", r.Acceptance[1])
	}
}

func TestPost_SpecValuesPreserveSemicolons(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{
		Subject: "split; but keep me together",
		Epic:    "campaign; phase one",
		Effort:  "review; verify",
		Acceptance: []string{
			"first; second; third",
		},
	})

	got := mustLoad(t, db, pid, q.ID)
	if got.Subject != "split; but keep me together" {
		t.Errorf("subject = %q", got.Subject)
	}
	if got.Epic != "campaign; phase one" {
		t.Errorf("epic = %q", got.Epic)
	}
	if got.Effort != "review; verify" {
		t.Errorf("effort = %q", got.Effort)
	}
	if len(got.Acceptance) != 1 || got.Acceptance[0] != "first; second; third" {
		t.Errorf("acceptance = %v", got.Acceptance)
	}
}

func TestPost_SemicolonFieldsRoundTrip(t *testing.T) {
	db, pid := newTestDB(t)
	q := mustPost(t, db, pid, PostParams{
		Subject:    "subject before; subject after",
		Epic:       "epic before; epic after",
		Effort:     "medium; weird but preserved",
		Acceptance: []string{"criterion before; criterion after"},
	})

	got := mustLoad(t, db, pid, q.ID)
	if got.Subject != "subject before; subject after" {
		t.Errorf("subject = %q", got.Subject)
	}
	if got.Epic != "epic before; epic after" {
		t.Errorf("epic = %q", got.Epic)
	}
	if got.Effort != "medium; weird but preserved" {
		t.Errorf("effort = %q", got.Effort)
	}
	if len(got.Acceptance) != 1 || got.Acceptance[0] != "criterion before; criterion after" {
		t.Errorf("acceptance = %v", got.Acceptance)
	}
}

func TestLoad_NotFound(t *testing.T) {
	db, pid := newTestDB(t)
	_, err := Load(context.Background(), db, pid, "QUEST-404")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
