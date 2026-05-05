package quest

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Regression for #23: "latest brief" reads were non-deterministic when
// two task_notes rows shared the same created_at string. The
// LastBrief / LastBriefAt queries used `ORDER BY created_at DESC LIMIT
// 1` only, so SQLite was free to return either tied row. Adding `id
// DESC` as a secondary sort key makes the highest-id (latest-inserted)
// row the unambiguous winner across runs.
//
// The test seeds five briefs with byte-identical RFC3339Nano
// timestamps, then reads the latest brief sameSecondBriefIterations
// times and asserts every read returns the same row (the one with the
// highest auto-increment id) — i.e. the most recently inserted brief.

const sameSecondBriefIterations = 10

func TestLastBrief_StableOrderForSameTimestampInserts(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	const n = 5
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("brief-%02d", i)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			pid, briefTaskID, "agent", briefPrefix+text, ts,
		); err != nil {
			t.Fatalf("seed brief %d: %v", i, err)
		}
	}

	// Highest auto-increment id == most recently inserted brief.
	const wantText = "brief-04"

	for i := 0; i < sameSecondBriefIterations; i++ {
		_, got, _, err := LastBrief(ctx, db, pid)
		if err != nil {
			t.Fatalf("LastBrief iter %d: %v", i, err)
		}
		if got != wantText {
			t.Fatalf("LastBrief iter %d: got %q, want %q", i, got, wantText)
		}
	}
}

func TestLastBriefAt_StableOrderForSameTimestampInserts(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	const n = 5
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("brief-%02d", i)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			pid, briefTaskID, "agent", briefPrefix+text, ts,
		); err != nil {
			t.Fatalf("seed brief %d: %v", i, err)
		}
	}

	first, err := LastBriefAt(ctx, db, pid)
	if err != nil {
		t.Fatalf("LastBriefAt: %v", err)
	}
	if first.IsZero() {
		t.Fatal("LastBriefAt returned zero time after seeding")
	}

	for i := 1; i < sameSecondBriefIterations; i++ {
		next, err := LastBriefAt(ctx, db, pid)
		if err != nil {
			t.Fatalf("LastBriefAt iter %d: %v", i, err)
		}
		if !next.Equal(first) {
			t.Fatalf("LastBriefAt iter %d: got %v, want %v", i, next, first)
		}
	}
}
