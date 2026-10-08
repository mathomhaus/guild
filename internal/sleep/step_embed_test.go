package sleep

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// embedTestModelID is the model identity stamped on test vector rows.
const embedTestModelID = "bge-small-en-v1.5-int8-cls"

// embedTestDeps wires a deterministic embedder, the fake the embed
// package ships for paths that need an embedder shape without ORT.
func embedTestDeps() *lore.EmbedDeps {
	return &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  embedTestModelID,
		Logger:   quietLogger(),
	}
}

// beginTestPass opens a pass row so direct step invocations have a
// valid PassID to journal against.
func beginTestPass(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id, err := BeginPass(ctx, db, TriggerAutopass, time.Minute, time.Time{})
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	return id
}

// seedLoreEntries inserts n active entries with distinct summaries,
// numbered from offset. Mirrors the embed package's test seeding.
func seedLoreEntries(t *testing.T, db *sql.DB, n, offset int) {
	t.Helper()
	ctx := context.Background()
	seedEmbeddingIdentity(t, db, embed.LoreCorpus{})
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for i := 0; i < n; i++ {
		idx := offset + i
		if _, err := db.ExecContext(ctx,
			`INSERT INTO entries (project_id, topic, kind, title, summary, tags, status)
			 VALUES ('p', 't', 'observation', ?, ?, '', 'current')`,
			"title-"+strconv.Itoa(idx), "summary body number "+strconv.Itoa(idx),
		); err != nil {
			t.Fatalf("seed entry %d: %v", idx, err)
		}
	}
}

// seedQuests inserts n quests with spec notes plus their
// tasks_fts_rows bridge rows, mirroring what quest_post does via
// trigger (and the embed package's QuestCorpus test seeding).
func seedQuests(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	ctx := context.Background()
	seedEmbeddingIdentity(t, db, embed.QuestCorpus{})
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for i := 1; i <= n; i++ {
		taskID := "QUEST-" + strconv.Itoa(i)
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
			taskID,
		); err != nil {
			t.Fatalf("seed task_status %s: %v", taskID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note)
			 VALUES ('p', ?, 'test', ?)`,
			taskID, "[spec] subject: quest number "+strconv.Itoa(i),
		); err != nil {
			t.Fatalf("seed task_notes %s: %v", taskID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO tasks_fts_rows (project_id,task_id) VALUES ('p',?)`,
			taskID,
		); err != nil {
			t.Fatalf("seed tasks_fts_rows %s: %v", taskID, err)
		}
	}
}

// countScalar runs a literal COUNT(*) query and returns the value.
func countScalar(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// vectorRow is one lore_vectors row snapshot for overwrite assertions.
type vectorRow struct {
	rowid       int64
	encodedAt   int64
	contentHash string
	vec         []byte
}

// readLoreVectorRows snapshots every lore_vectors row keyed by entry_id.
func readLoreVectorRows(t *testing.T, db *sql.DB) map[int64]vectorRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT rowid, entry_id, encoded_at, content_hash, vec FROM lore_vectors ORDER BY entry_id`)
	if err != nil {
		t.Fatalf("read lore_vectors: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]vectorRow{}
	for rows.Next() {
		var (
			r       vectorRow
			entryID int64
		)
		if err := rows.Scan(&r.rowid, &entryID, &r.encodedAt, &r.contentHash, &r.vec); err != nil {
			t.Fatalf("scan lore_vectors: %v", err)
		}
		out[entryID] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate lore_vectors: %v", err)
	}
	return out
}

// TestEmbedStep_BackfillsBothCorpora locks the happy path through the
// runner: pending entities in both corpora get embedded via
// embed.Backfill, and the journal carries one applied auto op per
// corpus with embedded/failed counts and an inverse payload.
func TestEmbedStep_BackfillsBothCorpora(t *testing.T) {
	ctx := context.Background()
	loreDB := openSleepDB(t)
	questDB := openSleepDB(t)
	seedLoreEntries(t, loreDB, 3, 0)
	seedQuests(t, questDB, 2)

	pc := &PassContext{
		LoreDB:  loreDB,
		QuestDB: questDB,
		Embed:   embedTestDeps(),
		Logger:  quietLogger(),
		Trigger: TriggerDaemonIdle,
	}
	res, err := Run(ctx, pc, []Step{EmbedStep{}}, 30*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Partial {
		t.Errorf("res.Partial = true, want false")
	}
	if len(res.Steps) != 1 || res.Steps[0].Err != nil {
		t.Fatalf("steps = %+v, want one clean step", res.Steps)
	}
	if got := res.Steps[0].Report.OpsApplied; got != 2 {
		t.Errorf("OpsApplied = %d, want 2 (one per corpus)", got)
	}

	if n := countScalar(t, loreDB, `SELECT COUNT(*) FROM lore_vectors`); n != 3 {
		t.Errorf("lore_vectors rows = %d, want 3", n)
	}
	if n := countScalar(t, questDB, `SELECT COUNT(*) FROM quest_vectors`); n != 2 {
		t.Errorf("quest_vectors rows = %d, want 2", n)
	}

	ops, err := PassOps(ctx, loreDB, res.PassID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("len(ops) = %d, want 2 (one auto op per corpus): %+v", len(ops), ops)
	}
	wantCounts := map[string]struct{ pending, embedded int64 }{
		"lore":  {pending: 3, embedded: 3},
		"quest": {pending: 2, embedded: 2},
	}
	for _, op := range ops {
		if op.Step != embedStepName || op.Kind != OpEmbedBackfill || op.Policy != PolicyAuto || !op.Applied {
			t.Errorf("op = %+v, want applied auto embed_backfill from step %q", op, embedStepName)
		}
		want, ok := wantCounts[op.Target]
		if !ok {
			t.Errorf("unexpected (or duplicate) op target %q", op.Target)
			continue
		}
		delete(wantCounts, op.Target)
		var detail embedOpDetail
		if err := json.Unmarshal([]byte(op.Detail), &detail); err != nil {
			t.Fatalf("unmarshal detail %q: %v", op.Detail, err)
		}
		if detail.Corpus != op.Target || detail.PendingBefore != want.pending ||
			int64(detail.Embedded) != want.embedded || detail.Failed != 0 {
			t.Errorf("detail = %+v, want corpus=%s pending_before=%d embedded=%d failed=0",
				detail, op.Target, want.pending, want.embedded)
		}
		if op.Inverse == "" {
			t.Errorf("op %s: empty inverse payload, want manual-reversal JSON", op.Target)
		}
	}
	if len(wantCounts) != 0 {
		t.Errorf("no op journaled for corpora: %v", wantCounts)
	}
}

// TestEmbedStep_ActsAboveCoverageFloor ensures an eligible missing vector
// is repaired even when existing coverage already clears the retrieval gate.
func TestEmbedStep_ActsAboveCoverageFloor(t *testing.T) {
	ctx := context.Background()
	loreDB := openSleepDB(t)
	seedLoreEntries(t, loreDB, 9, 0)

	// Pre-embed the first nine entries, then add a tenth: coverage sits
	// at 9/10 = 0.90. Maintenance must repair the remaining row.
	if _, err := embed.Backfill(ctx, embed.BackfillOptions{
		DB:       loreDB,
		Corpus:   embed.LoreCorpus{},
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  embedTestModelID,
		Logger:   quietLogger(),
	}); err != nil {
		t.Fatalf("pre-backfill: %v", err)
	}
	seedLoreEntries(t, loreDB, 1, 9)
	if n := countScalar(t, loreDB, `SELECT COUNT(*) FROM lore_vectors`); n != 9 {
		t.Fatalf("pre-state: lore_vectors rows = %d, want 9", n)
	}

	questDB := openSleepDB(t) // zero quests: quiet no-op for the quest corpus
	passID := beginTestPass(ctx, t, loreDB)
	pc := &PassContext{
		LoreDB:  loreDB,
		QuestDB: questDB,
		Embed:   embedTestDeps(),
		Logger:  quietLogger(),
		Trigger: TriggerAutopass,
		PassID:  passID,
	}
	report, err := EmbedStep{}.Run(ctx, pc)
	if err != nil {
		t.Fatalf("EmbedStep.Run: %v", err)
	}
	if report.OpsApplied != 1 {
		t.Errorf("OpsApplied = %d, want 1", report.OpsApplied)
	}
	if n := countScalar(t, loreDB, `SELECT COUNT(*) FROM lore_vectors`); n != 10 {
		t.Errorf("lore_vectors rows = %d, want 10 (step must act above the floor)", n)
	}

	ops, err := PassOps(ctx, loreDB, passID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("len(ops) = %d, want 1: %+v", len(ops), ops)
	}
	var detail embedOpDetail
	if err := json.Unmarshal([]byte(ops[0].Detail), &detail); err != nil {
		t.Fatalf("unmarshal detail %q: %v", ops[0].Detail, err)
	}
	if detail.Corpus != "lore" || detail.PendingBefore != 1 || detail.Embedded != 1 || detail.Failed != 0 {
		t.Errorf("detail = %+v, want corpus=lore pending_before=1 embedded=1 failed=0", detail)
	}
}

// TestEmbedStep_DisabledEmbedderJournalsSkip locks the degraded-input
// contract: nil or disabled EmbedDeps is a clean journaled no-op with
// the skip reason recorded, not an error.
func TestEmbedStep_DisabledEmbedderJournalsSkip(t *testing.T) {
	cases := []struct {
		name string
		deps *lore.EmbedDeps
	}{
		{name: "nil deps", deps: nil},
		{name: "disabled deps", deps: &lore.EmbedDeps{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openSleepDB(t)
			seedLoreEntries(t, db, 2, 0)
			passID := beginTestPass(ctx, t, db)
			pc := &PassContext{
				LoreDB:  db,
				Embed:   tc.deps,
				Logger:  quietLogger(),
				Trigger: TriggerAutopass,
				PassID:  passID,
			}
			report, err := EmbedStep{}.Run(ctx, pc)
			if err != nil {
				t.Fatalf("EmbedStep.Run: %v (want clean journaled no-op)", err)
			}
			if report.OpsApplied != 0 {
				t.Errorf("OpsApplied = %d, want 0", report.OpsApplied)
			}
			if n := countScalar(t, db, `SELECT COUNT(*) FROM lore_vectors`); n != 0 {
				t.Errorf("lore_vectors rows = %d, want 0", n)
			}

			ops, err := PassOps(ctx, db, passID)
			if err != nil {
				t.Fatalf("PassOps: %v", err)
			}
			if len(ops) != 1 {
				t.Fatalf("len(ops) = %d, want 1 skip row: %+v", len(ops), ops)
			}
			op := ops[0]
			if op.Step != embedStepName || op.Kind != OpEmbedBackfill || op.Policy != PolicyAuto || op.Applied {
				t.Errorf("op = %+v, want unapplied auto embed_backfill skip row", op)
			}
			var detail embedSkipDetail
			if err := json.Unmarshal([]byte(op.Detail), &detail); err != nil {
				t.Fatalf("unmarshal detail %q: %v", op.Detail, err)
			}
			if detail.Skipped != skipReasonEmbedderDisabled {
				t.Errorf("skip reason = %q, want %q", detail.Skipped, skipReasonEmbedderDisabled)
			}
		})
	}
}

// cancellingEmbedder embeds normally until trip calls have happened,
// then cancels the run's context before delegating, simulating the
// pass wall budget expiring mid-backfill.
type cancellingEmbedder struct {
	inner  embed.Embedder
	cancel context.CancelFunc
	calls  int
	trip   int
}

func (e *cancellingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.calls++
	if e.calls > e.trip {
		e.cancel()
	}
	return e.inner.Embed(ctx, text)
}

func (e *cancellingEmbedder) Dimension() int { return e.inner.Dimension() }

// TestEmbedStep_CancellationJournalsPartialProgress locks the budget
// contract: a cancellation mid-backfill surfaces the context error to
// the runner AND journals the partial progress made before the cut,
// through a cancellation-shielded journal write.
func TestEmbedStep_CancellationJournalsPartialProgress(t *testing.T) {
	db := openSleepDB(t)
	seedLoreEntries(t, db, 5, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	passID := beginTestPass(ctx, t, db)
	deps := &lore.EmbedDeps{
		Embedder: &cancellingEmbedder{
			inner:  embed.NewDeterministicEmbedder(),
			cancel: cancel,
			trip:   2,
		},
		ModelID: embedTestModelID,
		Logger:  quietLogger(),
	}
	pc := &PassContext{
		LoreDB:  db,
		Embed:   deps,
		Logger:  quietLogger(),
		Trigger: TriggerAutopass,
		PassID:  passID,
	}

	report, err := EmbedStep{}.Run(ctx, pc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EmbedStep.Run err = %v, want context.Canceled", err)
	}
	if report.OpsApplied != 1 {
		t.Errorf("OpsApplied = %d, want 1 (the partial lore op)", report.OpsApplied)
	}
	if n := countScalar(t, db, `SELECT COUNT(*) FROM lore_vectors`); n != 2 {
		t.Errorf("lore_vectors rows = %d, want 2 (progress before the cut)", n)
	}

	// Partial progress must be journaled despite the dead step context.
	ops, err := PassOps(context.Background(), db, passID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("len(ops) = %d, want 1 partial op (quest never started): %+v", len(ops), ops)
	}
	op := ops[0]
	if op.Target != "lore" || !op.Applied {
		t.Errorf("op = %+v, want applied lore op", op)
	}
	var detail embedOpDetail
	if err := json.Unmarshal([]byte(op.Detail), &detail); err != nil {
		t.Fatalf("unmarshal detail %q: %v", op.Detail, err)
	}
	if !detail.Cancelled {
		t.Errorf("detail.Cancelled = false, want true")
	}
	if detail.PendingBefore != 5 || detail.Embedded != 2 {
		t.Errorf("detail = %+v, want pending_before=5 embedded=2", detail)
	}
}

// TestEmbedStep_RerunPreservesFreshVectors ensures repair leaves valid,
// content-fresh rows untouched and journals no work for a healthy corpus.
func TestEmbedStep_RerunPreservesFreshVectors(t *testing.T) {
	ctx := context.Background()
	loreDB := openSleepDB(t)
	questDB := openSleepDB(t) // zero quests throughout
	seedLoreEntries(t, loreDB, 3, 0)

	runOnce := func() int64 {
		passID := beginTestPass(ctx, t, loreDB)
		pc := &PassContext{
			LoreDB:  loreDB,
			QuestDB: questDB,
			Embed:   embedTestDeps(),
			Logger:  quietLogger(),
			Trigger: TriggerAutopass,
			PassID:  passID,
		}
		if _, err := (EmbedStep{}).Run(ctx, pc); err != nil {
			t.Fatalf("EmbedStep.Run: %v", err)
		}
		if err := EndPass(ctx, loreDB, passID, time.Time{}); err != nil {
			t.Fatalf("EndPass: %v", err)
		}
		return passID
	}

	runOnce()
	before := readLoreVectorRows(t, loreDB)
	if len(before) != 3 {
		t.Fatalf("rows after first run = %d, want 3", len(before))
	}

	// Plant a timestamp sentinel without making the source hash stale.
	if _, err := loreDB.ExecContext(ctx,
		`UPDATE lore_vectors SET encoded_at = 123 WHERE entry_id = 1`); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	before = readLoreVectorRows(t, loreDB)
	// A new pending entity appears between passes; the second run must
	// add exactly one row and leave the originals untouched.
	seedLoreEntries(t, loreDB, 1, 3)
	runOnce()

	after := readLoreVectorRows(t, loreDB)
	if len(after) != 4 {
		t.Fatalf("rows after second run = %d, want 4", len(after))
	}
	for entryID, b := range before {
		a, ok := after[entryID]
		if !ok {
			t.Fatalf("entry %d vector row disappeared across reruns", entryID)
		}
		if a.rowid != b.rowid {
			t.Errorf("entry %d rowid changed across reruns: %d -> %d", entryID, b.rowid, a.rowid)
		}
		if a.encodedAt != b.encodedAt || !bytes.Equal(a.vec, b.vec) {
			t.Errorf("entry %d vector row rewritten across reruns", entryID)
		}
	}
	if after[1].encodedAt != 123 {
		t.Fatalf("fresh timestamp sentinel overwritten: %d", after[1].encodedAt)
	}

	// Third run with nothing pending anywhere: quiet no-op, no journal rows.
	passID := runOnce()
	ops, err := PassOps(ctx, loreDB, passID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("ops on a zero-pending pass = %+v, want none", ops)
	}
}

// TestEmbedStep_RespectsAutoOpCap locks the Caps guardrail: with
// MaxAutoOps = 1 and both corpora pending, only the first corpus is
// backfilled and the second is deferred to the next pass.
func TestEmbedStep_RespectsAutoOpCap(t *testing.T) {
	ctx := context.Background()
	loreDB := openSleepDB(t)
	questDB := openSleepDB(t)
	seedLoreEntries(t, loreDB, 2, 0)
	seedQuests(t, questDB, 2)
	passID := beginTestPass(ctx, t, loreDB)
	pc := &PassContext{
		LoreDB:  loreDB,
		QuestDB: questDB,
		Embed:   embedTestDeps(),
		Logger:  quietLogger(),
		Trigger: TriggerAutopass,
		PassID:  passID,
		Caps:    Caps{MaxAutoOps: 1},
	}

	report, err := EmbedStep{}.Run(ctx, pc)
	if err != nil {
		t.Fatalf("EmbedStep.Run: %v", err)
	}
	if report.OpsApplied != 1 {
		t.Errorf("OpsApplied = %d, want 1 (cap)", report.OpsApplied)
	}
	if n := countScalar(t, loreDB, `SELECT COUNT(*) FROM lore_vectors`); n != 2 {
		t.Errorf("lore_vectors rows = %d, want 2", n)
	}
	if n := countScalar(t, questDB, `SELECT COUNT(*) FROM quest_vectors`); n != 0 {
		t.Errorf("quest_vectors rows = %d, want 0 (deferred by cap)", n)
	}
	ops, err := PassOps(ctx, loreDB, passID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 1 || ops[0].Target != "lore" || !ops[0].Applied {
		t.Errorf("ops = %+v, want a single applied lore op", ops)
	}
}

// TestEmbedStep_NilQuestDBJournalsSkip locks the degraded-caller path:
// a missing quest handle is journaled as an unapplied skip row for the
// quest corpus while the lore corpus still backfills.
func TestEmbedStep_NilQuestDBJournalsSkip(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	seedLoreEntries(t, db, 1, 0)
	passID := beginTestPass(ctx, t, db)
	pc := &PassContext{
		LoreDB:  db,
		Embed:   embedTestDeps(),
		Logger:  quietLogger(),
		Trigger: TriggerAutopass,
		PassID:  passID,
	}

	report, err := EmbedStep{}.Run(ctx, pc)
	if err != nil {
		t.Fatalf("EmbedStep.Run: %v", err)
	}
	if report.OpsApplied != 1 {
		t.Errorf("OpsApplied = %d, want 1 (lore only)", report.OpsApplied)
	}

	ops, err := PassOps(ctx, db, passID)
	if err != nil {
		t.Fatalf("PassOps: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("len(ops) = %d, want 2 (lore op + quest skip): %+v", len(ops), ops)
	}
	if ops[0].Target != "lore" || !ops[0].Applied {
		t.Errorf("ops[0] = %+v, want applied lore op", ops[0])
	}
	if ops[1].Target != "quest" || ops[1].Applied {
		t.Errorf("ops[1] = %+v, want unapplied quest skip", ops[1])
	}
	var detail embedSkipDetail
	if err := json.Unmarshal([]byte(ops[1].Detail), &detail); err != nil {
		t.Fatalf("unmarshal detail %q: %v", ops[1].Detail, err)
	}
	if detail.Corpus != "quest" || detail.Skipped != skipReasonQuestDBNotWired {
		t.Errorf("skip detail = %+v, want corpus=quest skipped=%s", detail, skipReasonQuestDBNotWired)
	}
}

func seedEmbeddingIdentity(t *testing.T, db *sql.DB, corpus embed.VectorCorpus) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, corpus.MetaKey(embed.FieldEmbedderModelID), embedTestModelID); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedStepRepairsExistingVectors(t *testing.T) {
	for _, name := range []string{"lore", "quest"} {
		for _, damage := range []string{"stale-content", "wrong-model", "wrong-dim", "wrong-blob"} {
			t.Run(name+"/"+damage, func(t *testing.T) {
				ctx := context.Background()
				loreDB, questDB := openSleepDB(t), openSleepDB(t)
				db, corpus := loreDB, embed.VectorCorpus(embed.LoreCorpus{})
				if name == "lore" {
					seedLoreEntries(t, loreDB, 1, 0)
				} else {
					db = questDB
					corpus = embed.QuestCorpus{}
					seedQuests(t, questDB, 1)
				}
				if _, err := embed.Backfill(ctx, embed.BackfillOptions{DB: db, Corpus: corpus, Embedder: embed.NewDeterministicEmbedder(), ModelID: embedTestModelID}); err != nil {
					t.Fatal(err)
				}
				// Fixed table identifiers selected by this test, never user input.
				table := "lore_vectors"
				if name == "quest" {
					table = "quest_vectors"
				}
				assignment := "content_hash='older-source'"
				switch damage {
				case "wrong-model":
					assignment = "model_id='older-model'"
				case "wrong-dim":
					assignment = "dim=1"
				case "wrong-blob":
					assignment = "vec=X'01'"
				}
				if _, err := db.Exec(`UPDATE ` + table + ` SET ` + assignment); err != nil { //nolint:gosec,sqlcheck // compile-time test table and assignments
					t.Fatal(err)
				}
				before, err := embed.ReadCoverage(ctx, db, corpus, embedTestModelID, nil)
				if err != nil {
					t.Fatal(err)
				}
				if before.Eligible != 1 || before.Missing != 0 || before.Fresh != 0 {
					t.Fatalf("repair fixture=%+v", before)
				}
				if damage == "stale-content" && before.Valid != 1 {
					t.Fatal("stale fixture must have 100 percent model/shape coverage")
				}
				passID := beginTestPass(ctx, t, loreDB)
				report, err := (EmbedStep{}).Run(ctx, &PassContext{LoreDB: loreDB, QuestDB: questDB, Embed: embedTestDeps(), Logger: quietLogger(), PassID: passID, Trigger: TriggerAutopass})
				if err != nil {
					t.Fatal(err)
				}
				if report.OpsApplied != 1 {
					t.Fatalf("repair not applied: %+v", report)
				}
				after, err := embed.ReadCoverage(ctx, db, corpus, embedTestModelID, nil)
				if err != nil || after.Fresh != 1 {
					t.Fatalf("repair coverage=%+v err=%v", after, err)
				}
				ops, err := PassOps(ctx, loreDB, passID)
				if err != nil || len(ops) != 1 {
					t.Fatalf("repair journal=%+v err=%v", ops, err)
				}
				var detail embedOpDetail
				if err := json.Unmarshal([]byte(ops[0].Detail), &detail); err != nil {
					t.Fatal(err)
				}
				if detail.PendingBefore != 1 || detail.Embedded != 1 || detail.Skipped != 0 || detail.Failed != 0 {
					t.Fatalf("repair counts=%+v", detail)
				}
			})
		}
	}
}

type sourceEditingEmbedder struct{ db *sql.DB }

func (e sourceEditingEmbedder) Dimension() int { return embed.Dim }
func (e sourceEditingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if _, err := e.db.ExecContext(ctx, `UPDATE entries SET summary='edited during encode'`); err != nil {
		return nil, err
	}
	return embed.NewDeterministicEmbedder().Embed(ctx, text)
}

func TestEmbedStepJournalsConcurrentEditAsSkipped(t *testing.T) {
	ctx := context.Background()
	db := openSleepDB(t)
	seedLoreEntries(t, db, 1, 0)
	passID := beginTestPass(ctx, t, db)
	deps := embedTestDeps()
	deps.Embedder = sourceEditingEmbedder{db: db}
	report, err := (EmbedStep{}).Run(ctx, &PassContext{LoreDB: db, QuestDB: openSleepDB(t), Embed: deps, Logger: quietLogger(), PassID: passID, Trigger: TriggerAutopass})
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsApplied != 0 {
		t.Fatalf("stale encode marked applied: %+v", report)
	}
	ops, err := PassOps(ctx, db, passID)
	if err != nil || len(ops) != 1 {
		t.Fatalf("skip journal=%+v err=%v", ops, err)
	}
	if ops[0].Applied || ops[0].Inverse != "" {
		t.Fatal("skipped encode has mutation or inverse")
	}
	var detail embedOpDetail
	if err := json.Unmarshal([]byte(ops[0].Detail), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.PendingBefore != 1 || detail.Embedded != 0 || detail.Skipped != 1 || detail.Failed != 0 {
		t.Fatalf("skip counts=%+v", detail)
	}
}
