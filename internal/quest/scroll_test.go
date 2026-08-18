package quest

import (
	"context"
	"strings"
	"testing"
	"time"
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
