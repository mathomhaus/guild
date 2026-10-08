package sleep

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedPass inserts one ended pass with the given ops and returns its
// id. Ops get the pass id filled in; Step and Target default to
// placeholder values so callers only spell the fields under test.
func seedPass(t *testing.T, db *sql.DB, ops []Op) int64 {
	t.Helper()
	ctx := context.Background()
	started := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)

	passID, err := BeginPass(ctx, db, TriggerDaemonIdle, time.Minute, started)
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	for _, op := range ops {
		op.PassID = passID
		if op.Step == "" {
			op.Step = "test-step"
		}
		if op.Target == "" {
			op.Target = "LORE-1"
		}
		if _, err := RecordOp(ctx, db, op); err != nil {
			t.Fatalf("RecordOp: %v", err)
		}
	}
	if err := EndPass(ctx, db, passID, started.Add(time.Minute)); err != nil {
		t.Fatalf("EndPass: %v", err)
	}
	return passID
}

// TestSummarize_Segments locks the line format: verb-led segments,
// singular/plural handling, zero-count segments omitted, approval rows
// counted by policy verdict (not by approval_quest_post rows), batch
// embed rows weighted by Detail count, and the whole thing on one line.
func TestSummarize_Segments(t *testing.T) {
	passes := []Pass{{ID: 1}}
	ops := []Op{
		// Two auto melds applied.
		{Policy: PolicyAuto, Kind: OpMeldExactSupersede, Applied: true},
		{Policy: PolicyAuto, Kind: OpMeldExactSupersede, Applied: true},
		// One renewal bounty posted.
		{Policy: PolicyAuto, Kind: OpRenewalQuestPost, Applied: true},
		// Embed backfill: one single row + one batch row covering 4.
		{Policy: PolicyAuto, Kind: OpEmbedBackfill, Applied: true},
		{Policy: PolicyAuto, Kind: OpEmbedBackfill, Applied: true, Detail: `{"count": 4}`},
		// One gated proposal (the awaiting-review count source) plus the
		// approval quest post standing in for it; the pair must count
		// once, via the policy verdict.
		{Policy: PolicyApproval, Kind: OpNearMeld, Applied: false},
		{Policy: PolicyAuto, Kind: OpApprovalQuestPost, Applied: true},
		// One step the wall budget cut short.
		{Policy: PolicyAuto, Kind: opStepPartial, Applied: false},
	}

	line := Summarize(passes, ops)
	for _, want := range []string{
		"🌙 while you slept: ",
		"melded 2 entries",
		"posted 1 renewal bounty",
		"embedded 5 entries",
		"1 op awaiting review",
		"1 step deferred by budget",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q; got: %s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("narration must be a single line; got: %q", line)
	}
	// Exactly one pass: no pass-count suffix.
	if strings.Contains(line, "passes)") {
		t.Errorf("single-pass line must not carry a pass count; got: %s", line)
	}
}

func TestSummarize_SingularAndOmission(t *testing.T) {
	passes := []Pass{{ID: 1}}
	ops := []Op{
		{Policy: PolicyAuto, Kind: OpMeldExactSupersede, Applied: true},
	}
	line := Summarize(passes, ops)
	if want := "melded 1 entry"; !strings.Contains(line, want) {
		t.Errorf("line missing singular %q; got: %s", want, line)
	}
	// Every zero-count segment is omitted.
	for _, banned := range []string{"renewal", "embedded", "awaiting review", "deferred"} {
		if strings.Contains(line, banned) {
			t.Errorf("zero-count segment %q must be omitted; got: %s", banned, line)
		}
	}
}

func TestSummarize_MultiPassAndNoChanges(t *testing.T) {
	// No passes: nothing to say at all.
	if got := Summarize(nil, nil); got != "" {
		t.Errorf("Summarize(no passes) = %q, want empty", got)
	}

	// Two passes, zero countable ops: the line still appears (the
	// consumed flag was spent) and aggregates the pass count.
	line := Summarize([]Pass{{ID: 1}, {ID: 2}}, nil)
	if want := "🌙 while you slept (2 passes): no changes"; line != want {
		t.Errorf("Summarize = %q, want %q", line, want)
	}

	// Unapplied auto rows (e.g. step_error markers) don't count.
	line = Summarize([]Pass{{ID: 1}}, []Op{
		{Policy: PolicyAuto, Kind: opStepError, Applied: false},
		{Policy: PolicyAuto, Kind: OpMeldExactSupersede, Applied: false},
	})
	if want := "🌙 while you slept: no changes"; line != want {
		t.Errorf("Summarize = %q, want %q", line, want)
	}
}

// TestNarrate_ConsumesExactlyOnce drives the full consumed-flag flow:
// the first Narrate aggregates every ended pass into one line, the
// second returns nothing, and an in-flight pass is left alone for a
// later session.
func TestNarrate_ConsumesExactlyOnce(t *testing.T) {
	db := openSleepDB(t)
	ctx := context.Background()

	seedPass(t, db, []Op{
		{Policy: PolicyAuto, Kind: OpMeldExactSupersede, Applied: true},
		{Policy: PolicyApproval, Kind: OpSeal, Applied: false},
	})
	seedPass(t, db, []Op{
		{Policy: PolicyAuto, Kind: OpRenewalQuestPost, Applied: true},
	})
	// An in-flight pass (no EndPass): excluded from narration.
	if _, err := BeginPass(ctx, db, TriggerAutopass, time.Minute, time.Now().UTC()); err != nil {
		t.Fatalf("BeginPass (in-flight): %v", err)
	}

	line, err := Narrate(ctx, db, time.Time{})
	if err != nil {
		t.Fatalf("Narrate: %v", err)
	}
	for _, want := range []string{
		"(2 passes)",
		"melded 1 entry",
		"posted 1 renewal bounty",
		"1 op awaiting review",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("first narration missing %q; got: %s", want, line)
		}
	}

	// Second call: both ended passes are consumed, the in-flight pass
	// is still not narratable.
	again, err := Narrate(ctx, db, time.Time{})
	if err != nil {
		t.Fatalf("Narrate (second): %v", err)
	}
	if again != "" {
		t.Errorf("second narration = %q, want empty (consumed)", again)
	}
}

// TestNarrate_ConcurrentCallersNarrateOnce races callers at one
// unnarrated pass: exactly one caller gets the line, the others get
// ""; the guarded MarkNarrated claim splits passes without overlap.
func TestNarrate_ConcurrentCallersNarrateOnce(t *testing.T) {
	db := openSleepDB(t)
	seedPass(t, db, []Op{
		{Policy: PolicyAuto, Kind: OpMeldExactSupersede, Applied: true},
	})

	const callers = 8
	lines := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lines[i], errs[i] = Narrate(context.Background(), db, time.Time{})
		}(i)
	}
	wg.Wait()

	nonEmpty := 0
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if lines[i] != "" {
			nonEmpty++
			if !strings.Contains(lines[i], "melded 1 entry") {
				t.Errorf("caller %d line %q missing meld count", i, lines[i])
			}
		}
	}
	if nonEmpty != 1 {
		t.Errorf("pass narrated by %d callers, want exactly 1; lines: %q", nonEmpty, lines)
	}
}

func TestNarrate_Validation(t *testing.T) {
	if _, err := Narrate(context.Background(), nil, time.Time{}); err == nil {
		t.Error("Narrate(nil db) returned nil error")
	}

	// A journal-less DB (pre-migration) is an error from Narrate; the
	// MCP seam translates it to "no line". Locked here so the seam's
	// swallow stays a deliberate choice, not an accident.
	db := openSleepDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP TABLE sleep_ops`); err != nil {
		t.Fatalf("drop sleep_ops: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE sleep_passes`); err != nil {
		t.Fatalf("drop sleep_passes: %v", err)
	}
	if _, err := Narrate(ctx, db, time.Time{}); err == nil {
		t.Error("Narrate(journal-less db) returned nil error")
	}
}
