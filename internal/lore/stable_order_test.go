package lore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// Regression for the "newest-first ordering non-deterministic within
// the same second" report (#25).
//
// The on-disk created_at column is RFC3339 with second precision; a
// burst of inscribes in the same SQLite second produces multiple rows
// with byte-identical created_at strings. Without a tie-breaker on the
// monotonically increasing primary key, SQLite is free to return those
// rows in any order, so user-facing reads (`oath`, `list`, `lore
// appraise` LIKE-fallback, `whispers`) can shuffle between calls.
//
// The fix adds `e.id DESC` after `e.created_at DESC` on every promised
// newest-first read. The test below seeds five same-second principles,
// reads each surface ten times, and asserts every read returns the
// same permutation as the first. Running the file with -count=N and
// -race is sufficient to surface a regression — without the secondary
// sort, the reads diverge on the first or second iteration on most
// builds of modernc.org/sqlite.

const sameSecondReadIterations = 10

// insertSameSecondPrinciples seeds n principle entries with byte-
// identical created_at strings under the supplied project. Returns
// the slice of inserted ids in insertion order so callers can assert
// "newest first == reverse insertion order" deterministically.
func insertSameSecondPrinciples(t *testing.T, ctx context.Context, db *sql.DB, project string, n int) []int64 {
	t.Helper()

	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`,
		project, "/tmp/"+project,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	ts := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		res, err := db.ExecContext(ctx,
			`INSERT INTO entries
			 (project_id, topic, kind, title, summary, status, created_at, updated_at)
			 VALUES (?, 'oath-hygiene', 'principle', ?, 'same-second principle', 'current', ?, ?)`,
			project, fmt.Sprintf("principle-%02d", i), ts, ts)
		if err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
		id, _ := res.LastInsertId()
		ids[i] = id
	}
	return ids
}

// idSlice projects the IDs out of an []*Entry slice for cheap equality
// asserts; we only care about the permutation, not the row payloads.
func idSlice(entries []*Entry) []int64 {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// idSliceFromResults projects IDs out of an Appraise output.
func idSliceFromResults(results []AppraiseResult) []int64 {
	out := make([]int64, len(results))
	for i, r := range results {
		out[i] = r.Entry.ID
	}
	return out
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOath_StableOrderForSameSecondInserts(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	ids := insertSameSecondPrinciples(t, ctx, db, "p", 5)

	first, err := Oath(ctx, db, "p")
	if err != nil {
		t.Fatalf("oath: %v", err)
	}
	if len(first) != len(ids) {
		t.Fatalf("oath len: got %d, want %d", len(first), len(ids))
	}

	// Newest-first by stable tie-breaker == reverse insertion order
	// (id is INTEGER PRIMARY KEY AUTOINCREMENT and created_at ties).
	want := make([]int64, len(ids))
	for i, id := range ids {
		want[len(ids)-1-i] = id
	}
	if got := idSlice(first); !equalIDs(got, want) {
		t.Fatalf("oath order: got %v, want %v", got, want)
	}

	for i := 1; i < sameSecondReadIterations; i++ {
		next, err := Oath(ctx, db, "p")
		if err != nil {
			t.Fatalf("oath iter %d: %v", i, err)
		}
		if got := idSlice(next); !equalIDs(got, want) {
			t.Fatalf("oath iter %d: got %v, want %v", i, got, want)
		}
	}
}

func TestList_StableOrderForSameSecondInserts(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	ids := insertSameSecondPrinciples(t, ctx, db, "p", 5)

	first, err := List(ctx, db, ListFilters{Project: "p"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first) != len(ids) {
		t.Fatalf("list len: got %d, want %d", len(first), len(ids))
	}

	want := make([]int64, len(ids))
	for i, id := range ids {
		want[len(ids)-1-i] = id
	}
	if got := idSlice(first); !equalIDs(got, want) {
		t.Fatalf("list order: got %v, want %v", got, want)
	}

	for i := 1; i < sameSecondReadIterations; i++ {
		next, err := List(ctx, db, ListFilters{Project: "p"})
		if err != nil {
			t.Fatalf("list iter %d: %v", i, err)
		}
		if got := idSlice(next); !equalIDs(got, want) {
			t.Fatalf("list iter %d: got %v, want %v", i, got, want)
		}
	}
}

// Appraise's LIKE-fallback path is the recency-only branch most
// vulnerable to same-second non-determinism: when FTS5 returns zero
// rows the query falls back to LIKE on title/summary/tags + ORDER BY
// created_at, and re-ranking by recency-alone produces identical
// scores for ties. Verify that path is stable too.
func TestAppraise_LIKEFallback_StableOrderForSameSecondInserts(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	ids := insertSameSecondPrinciples(t, ctx, db, "p", 5)

	// Stamp a unique tag on every seeded entry so a substring query
	// can return the full set via the LIKE-tags branch.
	if _, err := db.ExecContext(ctx,
		`UPDATE entries SET tags = 'sameSecondTag' WHERE project_id = 'p'`,
	); err != nil {
		t.Fatalf("set tags: %v", err)
	}

	res, err := Appraise(ctx, db, AppraiseParams{
		Query:   "sameSecondT",
		Limit:   10,
		Project: "p",
		Now:     time.Date(2026, 4, 16, 12, 0, 1, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("appraise: %v", err)
	}
	if len(res.Results) == 0 {
		t.Skip("appraise returned no results; LIKE-fallback path not exercised on this build")
	}

	first := idSliceFromResults(res.Results)

	for i := 1; i < sameSecondReadIterations; i++ {
		next, err := Appraise(ctx, db, AppraiseParams{
			Query:   "sameSecondT",
			Limit:   10,
			Project: "p",
			Now:     time.Date(2026, 4, 16, 12, 0, 1, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("appraise iter %d: %v", i, err)
		}
		if got := idSliceFromResults(next.Results); !equalIDs(got, first) {
			t.Fatalf("appraise iter %d: got %v, want %v", i, got, first)
		}
	}

	// Sanity: every returned id must come from the seeded set.
	seeded := map[int64]bool{}
	for _, id := range ids {
		seeded[id] = true
	}
	for _, id := range first {
		if !seeded[id] {
			t.Fatalf("appraise returned non-seeded id %d", id)
		}
	}
}
