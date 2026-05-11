package quest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/command"
)

func TestScroll_FullHistory(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	q := mustPost(t, db, pid, PostParams{
		Subject:  "implement feature X",
		Priority: "P1",
	})

	// Add a journal note.
	if err := Journal(ctx, db, pid, q.ID, "bob", "started analysis"); err != nil {
		t.Fatalf("Journal: %v", err)
	}

	// Add a campfire snapshot.
	if err := Campfire(ctx, db, pid, q.ID, CampfireParams{
		Hypothesis: "it's in the cache layer",
		Tried:      []string{"cleared cache"},
		Next:       "inspect eviction logic",
	}); err != nil {
		t.Fatalf("Campfire: %v", err)
	}

	res, err := Scroll(ctx, db, pid, q.ID)
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}

	if res.Quest == nil {
		t.Fatal("quest is nil")
	}
	if res.Quest.ID != q.ID {
		t.Errorf("quest ID mismatch: got %q want %q", res.Quest.ID, q.ID)
	}
	if res.Quest.Subject != "implement feature X" {
		t.Errorf("wrong subject: %q", res.Quest.Subject)
	}

	// Notes: at minimum spec notes + journal + campfire.
	if len(res.Notes) == 0 {
		t.Error("expected at least one note")
	}

	// Find journal note.
	var foundJournal, foundCampfire bool
	for _, n := range res.Notes {
		if n.Note == "started analysis" {
			foundJournal = true
		}
		if strings.HasPrefix(n.Note, NotePrefixCheckpoint) && strings.Contains(n.Note, "cache layer") {
			foundCampfire = true
		}
	}
	if !foundJournal {
		t.Error("journal note not found in scroll")
	}
	if !foundCampfire {
		t.Error("campfire note not found in scroll")
	}

	// Events: at minimum the "created" event from Post.
	if len(res.Events) == 0 {
		t.Error("expected at least one event")
	}

	var foundCreated bool
	for _, e := range res.Events {
		if e.Event == EventCreated {
			foundCreated = true
		}
	}
	if !foundCreated {
		t.Errorf("%q event not found in scroll timeline", EventCreated)
	}
}

func TestScroll_NotFound(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	_, err := Scroll(ctx, db, pid, "QUEST-404")
	if err == nil {
		t.Fatal("expected error for missing quest")
	}
}

func TestScroll_DependencyStateExplainsBlockedQuest(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	done := mustPost(t, db, pid, PostParams{Subject: "finished setup"})
	open := mustPost(t, db, pid, PostParams{Subject: "remaining API work"})
	if _, err := Fulfill(ctx, db, pid, done.ID, "done"); err != nil {
		t.Fatalf("Fulfill done dep: %v", err)
	}
	blocked := mustPost(t, db, pid, PostParams{
		Subject:   "blocked integration",
		DependsOn: []string{done.ID, open.ID, "QUEST-404"},
	})
	if blocked.Status != StatusBlocked {
		t.Fatalf("blocked status = %s, want blocked", blocked.Status)
	}

	res, err := Scroll(ctx, db, pid, blocked.ID)
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}
	if len(res.Dependencies) != 3 {
		t.Fatalf("dependencies = %v, want 3", res.Dependencies)
	}
	checks := []struct {
		index   int
		id      string
		status  Status
		done    bool
		missing bool
		subject string
	}{
		{0, done.ID, StatusDone, true, false, "finished setup"},
		{1, open.ID, StatusNext, false, false, "remaining API work"},
		{2, "QUEST-404", "", false, true, ""},
	}
	for _, chk := range checks {
		got := res.Dependencies[chk.index]
		if got.ID != chk.id || got.Status != chk.status || got.Done != chk.done ||
			got.Missing != chk.missing || got.Subject != chk.subject {
			t.Fatalf("dependency[%d] = %+v, want id=%s status=%s done=%v missing=%v subject=%q",
				chk.index, got, chk.id, chk.status, chk.done, chk.missing, chk.subject)
		}
	}
}

func TestFormatScrollIncludesDependencyState(t *testing.T) {
	out := formatScrollMCP(command.MCPSink{}, ScrollOutput{Result: &ScrollResult{
		Quest: &Quest{
			ID:        "QUEST-3",
			Subject:   "blocked integration",
			Status:    StatusBlocked,
			Priority:  "P1",
			DependsOn: []string{"QUEST-1", "QUEST-2"},
		},
		Dependencies: []DependencyState{
			{ID: "QUEST-1", Status: StatusDone, Done: true, Subject: "finished setup"},
			{ID: "QUEST-2", Status: StatusNext, Subject: "remaining API work"},
		},
	}})
	if !strings.Contains(out, "dependencies:") {
		t.Fatalf("MCP scroll missing dependency section:\n%s", out)
	}
	if !strings.Contains(out, "QUEST-1 [done] finished setup") ||
		!strings.Contains(out, "QUEST-2 [next] remaining API work") {
		t.Fatalf("MCP scroll missing dependency detail:\n%s", out)
	}
}

func TestScroll_OrderChronological(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	q := mustPost(t, db, pid, PostParams{Subject: "ordering test"})

	for i, text := range []string{"note-1", "note-2", "note-3"} {
		_ = i
		if err := Journal(ctx, db, pid, q.ID, "agent", text); err != nil {
			t.Fatalf("Journal: %v", err)
		}
		// Sleep briefly to ensure distinct timestamps for each entry.
		// This guards against sub-second timestamp collisions that could
		// cause flaky ordering under -race.
		time.Sleep(time.Microsecond)
	}

	res, err := Scroll(ctx, db, pid, q.ID)
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}

	// Find our journal notes and verify they appear in order.
	var found []string
	for _, n := range res.Notes {
		if strings.HasPrefix(n.Note, "note-") {
			found = append(found, n.Note)
		}
	}
	if len(found) != 3 {
		t.Fatalf("expected 3 journal notes, got %d", len(found))
	}
	if found[0] != "note-1" || found[1] != "note-2" || found[2] != "note-3" {
		t.Errorf("notes not in chronological order: %v", found)
	}
}

func TestScroll_OrderChronologicalUsesIDTieBreaker(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	q := mustPost(t, db, pid, PostParams{Subject: "same timestamp ordering"})
	stamp := "2026-05-11T00:00:00Z"
	for _, note := range []string{"same-time-1", "same-time-2", "same-time-3"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			pid, q.ID, "agent", note, stamp,
		); err != nil {
			t.Fatalf("insert note %q: %v", note, err)
		}
	}

	res, err := Scroll(ctx, db, pid, q.ID)
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}

	var found []string
	for _, n := range res.Notes {
		if strings.HasPrefix(n.Note, "same-time-") {
			found = append(found, n.Note)
		}
	}
	want := []string{"same-time-1", "same-time-2", "same-time-3"}
	if strings.Join(found, ",") != strings.Join(want, ",") {
		t.Fatalf("same-timestamp notes = %v, want %v", found, want)
	}
}
