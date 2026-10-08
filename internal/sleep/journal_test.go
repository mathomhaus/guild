package sleep

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// openSleepDB opens a per-test file-backed SQLite DB under t.TempDir
// and applies the full migration chain. A tmpdir file (not :memory:)
// because storage rejects query-string DSNs and :memory: does not
// share across pooled connections; same rationale as the quest
// package's test helper.
func openSleepDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lore.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.MigrateTo(ctx, db, "", io.Discard); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestMigration009_AppliesAndRerunIsNoOp locks the migration contract:
// 009 applies cleanly to a fresh DB, creates both journal tables plus
// the pass_id index, and a second Migrate run is a no-op (exactly one
// schema_migrations row for version 9).
func TestMigration009_AppliesAndRerunIsNoOp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for run := 1; run <= 2; run++ {
		if err := storage.MigrateTo(ctx, db, "", io.Discard); err != nil {
			t.Fatalf("migrate run %d: %v", run, err)
		}
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 9`,
	).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != 1 {
		t.Errorf("schema_migrations rows for version 9 = %d, want 1", rows)
	}

	for _, obj := range []struct{ kind, name string }{
		{"table", "sleep_passes"},
		{"table", "sleep_ops"},
		{"index", "idx_sleep_ops_pass_id"},
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`,
			obj.kind, obj.name,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s %s: %v", obj.kind, obj.name, err)
		}
		if n != 1 {
			t.Errorf("%s %s: found %d, want 1", obj.kind, obj.name, n)
		}
	}
}

// TestJournal_RoundTrip drives the full write-then-narrate cycle:
// BeginPass + RecordOp (with inverse payload) + EndPass, then
// UnnarratedPasses surfaces the pass with every field intact, PassOps
// returns the op with the inverse payload round-tripped, and
// MarkNarrated removes the pass from the unnarrated set.
func TestJournal_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	started := time.Date(2026, 6, 12, 3, 0, 0, 0, time.UTC)
	budget := 90 * time.Second

	passID, err := BeginPass(ctx, db, TriggerDaemonIdle, budget, started)
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}

	// An in-flight pass must not surface as narratable.
	if got, err := UnnarratedPasses(ctx, db); err != nil || len(got) != 0 {
		t.Fatalf("UnnarratedPasses before EndPass = %v, %v; want empty, nil", got, err)
	}

	inverse := `{"prior_status":"current","edge":{"from":"LORE-40","to":"LORE-12","relation":"supersedes"}}`
	opID, err := RecordOp(ctx, db, Op{
		PassID:  passID,
		Step:    "consolidation",
		Policy:  PolicyAuto,
		Kind:    OpMeldExactSupersede,
		Target:  "LORE-12<-LORE-40",
		Detail:  `{"score":1.0}`,
		Inverse: inverse,
		Applied: true,
	})
	if err != nil {
		t.Fatalf("RecordOp: %v", err)
	}
	if opID <= 0 {
		t.Fatalf("RecordOp id = %d, want > 0", opID)
	}

	ended := started.Add(5 * time.Second)
	if err := EndPass(ctx, db, passID, ended); err != nil {
		t.Fatalf("EndPass: %v", err)
	}

	passes, err := UnnarratedPasses(ctx, db)
	if err != nil {
		t.Fatalf("UnnarratedPasses: %v", err)
	}
	if len(passes) != 1 {
		t.Fatalf("UnnarratedPasses len = %d, want 1", len(passes))
	}
	p := passes[0]
	if p.ID != passID {
		t.Errorf("pass id = %d, want %d", p.ID, passID)
	}
	if !p.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", p.StartedAt, started)
	}
	if !p.EndedAt.Equal(ended) {
		t.Errorf("ended_at = %v, want %v", p.EndedAt, ended)
	}
	if p.Trigger != TriggerDaemonIdle {
		t.Errorf("trigger = %q, want %q", p.Trigger, TriggerDaemonIdle)
	}
	if p.Budget != budget {
		t.Errorf("budget = %v, want %v", p.Budget, budget)
	}
	if !p.NarratedAt.IsZero() {
		t.Errorf("narrated_at = %v, want zero", p.NarratedAt)
	}

	ops, err := PassOps(ctx, db, passID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("PassOps len = %d, want 1", len(ops))
	}
	op := ops[0]
	if op.ID != opID || op.PassID != passID {
		t.Errorf("op ids = (%d, %d), want (%d, %d)", op.ID, op.PassID, opID, passID)
	}
	if op.Step != "consolidation" || op.Policy != PolicyAuto || op.Kind != OpMeldExactSupersede {
		t.Errorf("op identity = (%q, %q, %q), want (consolidation, auto, meld_exact_supersede)",
			op.Step, op.Policy, op.Kind)
	}
	if op.Target != "LORE-12<-LORE-40" {
		t.Errorf("op target = %q, want LORE-12<-LORE-40", op.Target)
	}
	if op.Inverse != inverse {
		t.Errorf("op inverse = %q, want %q", op.Inverse, inverse)
	}
	if !op.Applied {
		t.Errorf("op applied = false, want true")
	}

	marked, err := MarkNarrated(ctx, db, passID, ended.Add(time.Minute))
	if err != nil {
		t.Fatalf("MarkNarrated: %v", err)
	}
	if !marked {
		t.Fatalf("MarkNarrated = false, want true")
	}
	if got, err := UnnarratedPasses(ctx, db); err != nil || len(got) != 0 {
		t.Fatalf("UnnarratedPasses after MarkNarrated = %v, %v; want empty, nil", got, err)
	}
}

// TestMarkNarrated_RacersMarkExactlyOnce proves the WHERE narrated_at
// IS NULL guard: many goroutines racing MarkNarrated on the same pass
// produce exactly one winner.
func TestMarkNarrated_RacersMarkExactlyOnce(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	now := time.Date(2026, 6, 12, 4, 0, 0, 0, time.UTC)

	passID, err := BeginPass(ctx, db, TriggerAutopass, time.Minute, now)
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	if err := EndPass(ctx, db, passID, now.Add(time.Second)); err != nil {
		t.Fatalf("EndPass: %v", err)
	}

	const racers = 8
	wins := make(chan bool, racers)
	errs := make(chan error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := MarkNarrated(ctx, db, passID, now.Add(2*time.Second))
			if err != nil {
				errs <- err
				return
			}
			wins <- ok
		}()
	}
	wg.Wait()
	close(wins)
	close(errs)

	for err := range errs {
		t.Fatalf("MarkNarrated: %v", err)
	}
	won := 0
	for ok := range wins {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Errorf("winning MarkNarrated calls = %d, want exactly 1", won)
	}
}

// TestEndPass_Semantics locks the idempotency and not-found contracts:
// re-ending keeps the original timestamp, ending a phantom pass errors.
func TestEndPass_Semantics(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	now := time.Date(2026, 6, 12, 5, 0, 0, 0, time.UTC)

	passID, err := BeginPass(ctx, db, TriggerDaemonIdle, time.Minute, now)
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	first := now.Add(time.Second)
	if err := EndPass(ctx, db, passID, first); err != nil {
		t.Fatalf("EndPass: %v", err)
	}
	// Second end is a no-op; the original ended_at wins.
	if err := EndPass(ctx, db, passID, now.Add(time.Hour)); err != nil {
		t.Fatalf("EndPass rerun: %v", err)
	}
	passes, err := UnnarratedPasses(ctx, db)
	if err != nil || len(passes) != 1 {
		t.Fatalf("UnnarratedPasses = %v, %v; want one pass", passes, err)
	}
	if !passes[0].EndedAt.Equal(first) {
		t.Errorf("ended_at = %v, want original %v", passes[0].EndedAt, first)
	}

	if err := EndPass(ctx, db, 99999, now); err == nil {
		t.Errorf("EndPass on phantom pass: want error, got nil")
	}
}

// TestLastPassEndedAt covers the scheduler read: no completed pass
// yields ok=false; afterwards the newest ended_at wins.
func TestLastPassEndedAt(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)

	if _, ok, err := LastPassEndedAt(ctx, db); err != nil || ok {
		t.Fatalf("LastPassEndedAt on empty journal = ok=%v, err=%v; want ok=false, nil", ok, err)
	}

	base := time.Date(2026, 6, 12, 6, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		start := base.Add(time.Duration(i) * time.Hour)
		id, err := BeginPass(ctx, db, TriggerDaemonIdle, time.Minute, start)
		if err != nil {
			t.Fatalf("BeginPass %d: %v", i, err)
		}
		if err := EndPass(ctx, db, id, start.Add(time.Minute)); err != nil {
			t.Fatalf("EndPass %d: %v", i, err)
		}
	}

	got, ok, err := LastPassEndedAt(ctx, db)
	if err != nil || !ok {
		t.Fatalf("LastPassEndedAt = ok=%v, err=%v; want ok=true, nil", ok, err)
	}
	want := base.Add(time.Hour + time.Minute)
	if !got.Equal(want) {
		t.Errorf("LastPassEndedAt = %v, want %v", got, want)
	}
}

// TestJournal_WriteValidation locks the cheap-error paths so a
// misconfigured step fails loudly instead of tripping a raw CHECK
// constraint.
func TestJournal_WriteValidation(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	now := time.Date(2026, 6, 12, 7, 0, 0, 0, time.UTC)

	if _, err := BeginPass(ctx, db, Trigger("cron"), time.Minute, now); err == nil {
		t.Errorf("BeginPass with invalid trigger: want error, got nil")
	}
	if _, err := BeginPass(ctx, db, TriggerDaemonIdle, 0, now); err == nil {
		t.Errorf("BeginPass with zero budget: want error, got nil")
	}

	passID, err := BeginPass(ctx, db, TriggerDaemonIdle, time.Minute, now)
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	bad := []Op{
		{Step: "s", Policy: PolicyAuto, Kind: OpEmbedBackfill, Target: "t"},                      // no pass id
		{PassID: passID, Policy: PolicyAuto, Kind: OpEmbedBackfill, Target: "t"},                 // no step
		{PassID: passID, Step: "s", Policy: Policy("maybe"), Kind: OpEmbedBackfill, Target: "t"}, // bad policy
		{PassID: passID, Step: "s", Policy: PolicyAuto, Target: "t"},                             // no kind
		{PassID: passID, Step: "s", Policy: PolicyAuto, Kind: OpEmbedBackfill},                   // no target
	}
	for i, op := range bad {
		if _, err := RecordOp(ctx, db, op); err == nil {
			t.Errorf("RecordOp bad case %d: want error, got nil", i)
		}
	}
}
