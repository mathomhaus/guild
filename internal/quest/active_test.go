package quest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
)

func TestActive_None(t *testing.T) {
	db, _ := newTestDB(t)

	qs, err := Active(context.Background(), db, "")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(qs) != 0 {
		t.Fatalf("want 0, got %d", len(qs))
	}
}

func TestActive_CrossProject(t *testing.T) {
	db, pid1 := newTestDB(t)
	ctx := context.Background()

	// Register a second project.
	pid2 := "testproj2"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
		pid2, t.TempDir(), "TASKS.md",
	); err != nil {
		t.Fatalf("register project2: %v", err)
	}

	// Post and accept in each project.
	q1 := mustPost(t, db, pid1, PostParams{Subject: "proj1 task"})
	q2 := mustPost(t, db, pid2, PostParams{Subject: "proj2 task"})

	if _, err := Accept(ctx, db, pid1, q1.ID, "agent-a"); err != nil {
		t.Fatalf("accept1: %v", err)
	}
	if _, err := Accept(ctx, db, pid2, q2.ID, "agent-b"); err != nil {
		t.Fatalf("accept2: %v", err)
	}

	qs, err := Active(ctx, db, "")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("want 2 active, got %d", len(qs))
	}

	for _, q := range qs {
		if q.Status != StatusInProgress {
			t.Errorf("quest %s: status = %s, want in_progress", q.ID, q.Status)
		}
	}
}

func TestActive_ExcludesNonInProgress(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	q1 := mustPost(t, db, pid, PostParams{Subject: "active"})
	mustPost(t, db, pid, PostParams{Subject: "waiting"})

	if _, err := Accept(ctx, db, pid, q1.ID, "agent"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	qs, err := Active(ctx, db, "")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("want 1, got %d", len(qs))
	}
	if qs[0].ID != q1.ID {
		t.Errorf("wrong quest: got %s, want %s", qs[0].ID, q1.ID)
	}
}

func TestActiveCommand_ProjectFlagFilters(t *testing.T) {
	db, pid1 := newTestDB(t)
	ctx := context.Background()

	pid2 := "testproj2"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
		pid2, t.TempDir(), "TASKS.md",
	); err != nil {
		t.Fatalf("register project2: %v", err)
	}

	q1 := mustPost(t, db, pid1, PostParams{Subject: "proj1 task"})
	q2 := mustPost(t, db, pid2, PostParams{Subject: "proj2 task"})
	if _, err := Accept(ctx, db, pid1, q1.ID, "agent-a"); err != nil {
		t.Fatalf("accept1: %v", err)
	}
	if _, err := Accept(ctx, db, pid2, q2.ID, "agent-b"); err != nil {
		t.Fatalf("accept2: %v", err)
	}

	deps := command.Deps{
		OpenDB:      func(_ context.Context) (*sql.DB, error) { return db, nil },
		ResolveProj: func(_ context.Context, arg string) (string, error) { return arg, nil },
	}
	out, err := ActiveCommand.Handler(ctx, deps, ActiveInput{Project: pid2})
	if err != nil {
		t.Fatalf("ActiveCommand handler: %v", err)
	}
	if len(out.Quests) != 1 {
		t.Fatalf("want 1 active quest in %s, got %d", pid2, len(out.Quests))
	}
	if out.Quests[0].Subject != "proj2 task" {
		t.Errorf("wrong quest: got %q, want %q", out.Quests[0].Subject, "proj2 task")
	}
}
