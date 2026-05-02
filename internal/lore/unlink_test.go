package lore

import (
	"context"
	"errors"
	"testing"
)

// TestUnlinkEntries_RemovesEdge creates a forward informs edge, calls
// UnlinkEntries, then asserts the edge is gone from the DB.
func TestUnlinkEntries_RemovesEdge(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindPrinciple,
		Title: "source entry for unlink removal test", Summary: "s", Topic: "x",
	})
	b, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindDecision,
		Title: "target entry for unlink removal test", Summary: "s", Topic: "x",
	})

	if err := LinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms); err != nil {
		t.Fatalf("link: %v", err)
	}

	res, err := UnlinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms)
	if err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if !res.Removed {
		t.Errorf("want Removed=true, got false; note=%q", res.Note)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entry_links WHERE from_id = ? AND to_id = ?`,
		a.Entry.ID, b.Entry.ID,
	).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if n != 0 {
		t.Errorf("want 0 link rows after unlink, got %d", n)
	}
}

// TestUnlinkEntries_Idempotent verifies that unlinking a non-existent edge
// returns success with Removed=false rather than an error.
func TestUnlinkEntries_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "source entry for idempotent unlink test", Summary: "s", Topic: "x",
	})
	b, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "target entry for idempotent unlink test", Summary: "s", Topic: "x",
	})

	// No link exists, must succeed with no-op result.
	res, err := UnlinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms)
	if err != nil {
		t.Fatalf("unlink (no edge): %v", err)
	}
	if res.Removed {
		t.Errorf("want Removed=false for non-existent edge, got true")
	}
	if res.Note != "no matching edge" {
		t.Errorf("want note=%q, got %q", "no matching edge", res.Note)
	}
}

// TestUnlinkEntries_SecondCallIdempotent creates an edge, removes it, then
// calls unlink a second time and confirms it still returns success.
func TestUnlinkEntries_SecondCallIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "source entry for double unlink test", Summary: "s", Topic: "x",
	})
	b, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "target entry for double unlink test", Summary: "s", Topic: "x",
	})

	if err := LinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := UnlinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms); err != nil {
		t.Fatalf("unlink 1: %v", err)
	}
	res, err := UnlinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms)
	if err != nil {
		t.Fatalf("unlink 2 (should be no-op): %v", err)
	}
	if res.Removed {
		t.Errorf("want Removed=false on second unlink, got true")
	}
}

// TestUnlinkEntries_CrossProject verifies that a cross-project informs edge
// can be removed via UnlinkEntries.
func TestUnlinkEntries_CrossProject(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha", "beta")

	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindPrinciple,
		Title: "alpha principle for cross-project unlink test", Summary: "s", Topic: "x",
	})
	b, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "beta", Kind: KindDecision,
		Title: "beta decision for cross-project unlink test", Summary: "s", Topic: "x",
	})

	if err := LinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms); err != nil {
		t.Fatalf("link: %v", err)
	}
	res, err := UnlinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms)
	if err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if !res.Removed {
		t.Errorf("want Removed=true for cross-project edge, got false")
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entry_links WHERE from_id = ? AND to_id = ?`,
		a.Entry.ID, b.Entry.ID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("want 0 rows after cross-project unlink, got %d", n)
	}
}

// TestUnlinkEntries_InvalidRelation verifies that an unknown relation string
// is rejected before any DB mutation occurs.
func TestUnlinkEntries_InvalidRelation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "entry for invalid relation unlink test", Summary: "s", Topic: "x",
	})
	b, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "entry for invalid relation unlink test target", Summary: "s", Topic: "x",
	})

	_, err := UnlinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, Relation("loves"))
	if !errors.Is(err, ErrInvalidRelation) {
		t.Errorf("want ErrInvalidRelation, got %v", err)
	}
}

// TestUnlinkEntries_SelfLink verifies that fromID==toID is rejected.
func TestUnlinkEntries_SelfLink(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "entry for self-unlink rejection test", Summary: "s", Topic: "x",
	})

	_, err := UnlinkEntries(ctx, db, a.Entry.ID, a.Entry.ID, RelationInforms)
	if !errors.Is(err, ErrSelfLink) {
		t.Errorf("want ErrSelfLink, got %v", err)
	}
}

// TestUnlinkEntries_MissingFrom verifies that a non-existent from_id is
// rejected with ErrEntryNotFound.
func TestUnlinkEntries_MissingFrom(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	b, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "target entry for missing-from unlink test", Summary: "s", Topic: "x",
	})

	_, err := UnlinkEntries(ctx, db, 9999, b.Entry.ID, RelationInforms)
	if !errors.Is(err, ErrEntryNotFound) {
		t.Errorf("want ErrEntryNotFound, got %v", err)
	}
}

// TestUnlinkEntries_WrongRelationIsNoOp verifies that calling unlink with a
// relation that does not match the stored edge returns Removed=false and
// leaves the stored edge intact. The entry_links table has a composite PK of
// (from_id, to_id) with the relation stored as a plain column, so the DELETE
// WHERE clause must match all three columns.
func TestUnlinkEntries_WrongRelationIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	a, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "source entry for wrong-relation unlink test", Summary: "s", Topic: "x",
	})
	b, _ := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha", Kind: KindIdea,
		Title: "target entry for wrong-relation unlink test", Summary: "s", Topic: "x",
	})

	// Create an informs edge.
	if err := LinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationInforms); err != nil {
		t.Fatalf("link informs: %v", err)
	}

	// Try to unlink using a different relation, must be a no-op.
	res, err := UnlinkEntries(ctx, db, a.Entry.ID, b.Entry.ID, RelationSupersedes)
	if err != nil {
		t.Fatalf("unlink supersedes (no-op): %v", err)
	}
	if res.Removed {
		t.Errorf("want Removed=false when relation does not match, got true")
	}

	// The informs edge must still exist.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entry_links WHERE from_id = ? AND to_id = ? AND relation = 'informs'`,
		a.Entry.ID, b.Entry.ID,
	).Scan(&n); err != nil {
		t.Fatalf("count informs: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 informs edge remaining after wrong-relation unlink, got %d", n)
	}
}
