package lore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// readMetaInt64 reads a meta table value as int64.
// Returns 0 when the key is absent or unparseable.
func readMetaInt64(t *testing.T, db *sql.DB, key string) int64 {
	t.Helper()
	var s sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("readMetaInt64(%q): %v", key, err)
	}
	if !s.Valid || s.String == "" {
		return 0
	}
	n, err := strconv.ParseInt(s.String, 10, 64)
	if err != nil {
		t.Fatalf("readMetaInt64(%q): parse %q: %v", key, s.String, err)
	}
	return n
}

// TestCoverageCounter_Inscribe verifies that each Inscribe call increments
// vector_coverage_den by exactly 1 regardless of kind.
func TestCoverageCounter_Inscribe(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")

	kinds := AllKinds()
	for i, k := range kinds {
		_, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID: "p",
			Kind:      k,
			Title:     "entry " + strconv.Itoa(i),
			Summary:   "summary for entry " + strconv.Itoa(i),
			Topic:     "test",
		})
		if err != nil {
			t.Fatalf("inscribe kind=%s: %v", k, err)
		}
		got := readMetaInt64(t, db, "vector_coverage_den")
		want := int64(i + 1)
		if got != want {
			t.Errorf("after inscribe #%d (kind=%s): den=%d want %d", i+1, k, got, want)
		}
	}
}

// TestCoverageCounter_Seal verifies that Seal decrements den once and that
// re-sealing the same entry does not double-decrement.
func TestCoverageCounter_Seal(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p2")

	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "p2",
		Kind:      KindObservation,
		Title:     "observation to seal",
		Summary:   "summary of observation",
		Topic:     "test",
	})
	if err != nil {
		t.Fatalf("inscribe: %v", err)
	}
	id := res.Entry.ID

	if got := readMetaInt64(t, db, "vector_coverage_den"); got != 1 {
		t.Fatalf("den after inscribe: got %d want 1", got)
	}

	// First seal: should decrement den to 0.
	sealNow := time.Now().UTC()
	if _, err := Seal(ctx, db, id, "p2", sealNow); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if got := readMetaInt64(t, db, "vector_coverage_den"); got != 0 {
		t.Errorf("den after first seal: got %d want 0", got)
	}

	// Re-seal: already archived, must NOT decrement den again (floor 0 stays 0).
	if _, err := Seal(ctx, db, id, "p2", sealNow); err != nil {
		t.Fatalf("re-seal: %v", err)
	}
	if got := readMetaInt64(t, db, "vector_coverage_den"); got != 0 {
		t.Errorf("den after re-seal: got %d want 0 (no double-decrement)", got)
	}
}

// TestCoverageCounter_Restore verifies that Restore increments den only for
// active (not archived, not parked) entries.
func TestCoverageCounter_Restore(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p3")

	snapPath := filepath.Join(t.TempDir(), "snap.json")
	writeTestSnapshot(t, snapPath, 1, []snapshotLoreEntry{
		{ID: 1, Topic: "t", Kind: "observation", Title: "active entry", Summary: "summary", Status: "current"},
		{ID: 2, Topic: "t", Kind: "observation", Title: "archived entry", Summary: "summary", Status: "archived"},
		{ID: 3, Topic: "t", Kind: "observation", Title: "parked entry", Summary: "summary", Status: "parked"},
		{ID: 4, Topic: "t", Kind: "idea", Title: "another active entry", Summary: "summary", Status: "seed"},
	}, nil)

	result, err := Restore(ctx, db, "p3", snapPath)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if result.Imported != 4 {
		t.Fatalf("imported: got %d want 4", result.Imported)
	}

	// Only 2 entries are active (current + seed). archived and parked are excluded.
	if got := readMetaInt64(t, db, "vector_coverage_den"); got != 2 {
		t.Errorf("den after restore: got %d want 2", got)
	}
}

// TestCoverageCounter_InscribeSealRestore exercises the full state machine:
// inscribe -> seal -> restore, confirming den stays consistent.
func TestCoverageCounter_InscribeSealRestore(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p4")

	// Inscribe 3 entries: den should be 3.
	var ids []int64
	for i := 0; i < 3; i++ {
		r, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID: "p4",
			Kind:      KindResearch,
			Title:     "entry " + strconv.Itoa(i),
			Summary:   "summary " + strconv.Itoa(i),
			Topic:     "test",
		})
		if err != nil {
			t.Fatalf("inscribe %d: %v", i, err)
		}
		ids = append(ids, r.Entry.ID)
	}
	if got := readMetaInt64(t, db, "vector_coverage_den"); got != 3 {
		t.Fatalf("den after 3 inscribes: got %d want 3", got)
	}

	// Seal one: den should be 2.
	if _, err := Seal(ctx, db, ids[0], "p4", time.Now().UTC()); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if got := readMetaInt64(t, db, "vector_coverage_den"); got != 2 {
		t.Errorf("den after seal: got %d want 2", got)
	}

	// Restore 2 active + 1 archived entries: den += 2 (only active).
	snapPath := filepath.Join(t.TempDir(), "snap2.json")
	writeTestSnapshot(t, snapPath, 1, []snapshotLoreEntry{
		{ID: 100, Topic: "t2", Kind: "observation", Title: "restored active 1", Summary: "s", Status: "current"},
		{ID: 101, Topic: "t2", Kind: "observation", Title: "restored active 2", Summary: "s", Status: "current"},
		{ID: 102, Topic: "t2", Kind: "observation", Title: "restored archived", Summary: "s", Status: "archived"},
	}, nil)
	if _, err := Restore(ctx, db, "p4", snapPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// den = 2 (after seal) + 2 (restored active) = 4.
	if got := readMetaInt64(t, db, "vector_coverage_den"); got != 4 {
		t.Errorf("den after restore: got %d want 4", got)
	}
}
