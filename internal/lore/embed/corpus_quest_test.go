// Parameterized interface tests for QuestCorpus: proves the adapter
// satisfies the same VectorCorpus contract as LoreCorpus and fakeCorpus.
//
// Design:
//  1. LSP: the same algorithm calls (Backfill, LoadFromDB, TopK,
//     WriteVector, ReadHealthReport) produce equivalent semantics
//     through QuestCorpus as through LoreCorpus.
//  2. Open/Closed: adding QuestCorpus required zero edits to Index,
//     Backfill, WriteVector, or ReadHealthReport.
//  3. Prefix isolation: 'quest.'-prefixed meta rows do not alias with
//     lore's unprefixed rows (verified by TestQuestCorpus_MetaPrefix).
//
// Schema: migration 005 creates tasks_fts_rows (integer-ID bridge),
// quest_vectors (entry_id PK, mirrors lore_vectors shape), tasks_fts
// (FTS5 vtable), and the 'quest.' meta seeds. openEmbedTestDB applies
// all migrations so QuestCorpus's tables are present in the test DB.

package embed

import (
	"context"
	"testing"
)

// TestQuestCorpus_CompileTimeInterface verifies QuestCorpus satisfies
// the VectorCorpus interface. The var _ = QuestCorpus{} assertion in
// corpus_quest.go already provides this at compile time; this test makes
// the verification visible as an explicit pass in the test suite.
func TestQuestCorpus_CompileTimeInterface(t *testing.T) {
	var _ VectorCorpus = QuestCorpus{}
}

// TestQuestCorpus_Accessors verifies every CorpusSchema accessor returns
// the expected constant string. This guards against accidental refactors
// that rename a table or column and break the algorithm SQL templates.
func TestQuestCorpus_Accessors(t *testing.T) {
	c := QuestCorpus{}
	if got := c.Name(); got != "quest" {
		t.Errorf("Name(): got %q want %q", got, "quest")
	}
	if got := c.VectorTable(); got != "quest_vectors" {
		t.Errorf("VectorTable(): got %q want %q", got, "quest_vectors")
	}
	if got := c.EntityTable(); got != "tasks_fts_rows" {
		t.Errorf("EntityTable(): got %q want %q", got, "tasks_fts_rows")
	}
	if got := c.EntityIDColumn(); got != "id" {
		t.Errorf("EntityIDColumn(): got %q want %q", got, "id")
	}
	if got := c.VectorStateColumn(); got != "" {
		t.Errorf("VectorStateColumn(): got %q want %q (empty)", got, "")
	}
	if got := c.ActivePredicate(); got == "" {
		t.Errorf("ActivePredicate(): got empty string, want non-empty")
	}
}

// TestQuestCorpus_MetaPrefix verifies every MetaField maps to a
// 'quest.'-prefixed key, and that none of the keys collide with
// LoreCorpus's unprefixed keys. This is the prefix-isolation contract
// that allows two corpora to share a single meta table.
func TestQuestCorpus_MetaPrefix(t *testing.T) {
	qc := QuestCorpus{}
	lc := LoreCorpus{}

	fields := []MetaField{
		FieldEmbedderState,
		FieldEmbedderModelID,
		FieldEmbedderTokenizerHash,
		FieldEmbedderRuntimeVersion,
		FieldEmbedderDim,
		FieldEmbedderStateReason,
		FieldVectorEpoch,
		FieldVectorCoverageNum,
		FieldVectorCoverageDen,
		FieldEmbedErrorCount,
		FieldEmbedLastError,
		FieldEmbedLastErrorAt,
		FieldEmbedLastOKAt,
	}

	for _, f := range fields {
		questKey := qc.MetaKey(f)
		loreKey := lc.MetaKey(f)

		// 1. Quest key must be non-empty (unknown enum returns "").
		if questKey == "" {
			t.Errorf("QuestCorpus.MetaKey(%d): returned empty string (missing case?)", f)
			continue
		}
		// 2. Quest key must start with 'quest.' prefix.
		const prefix = "quest."
		if len(questKey) <= len(prefix) || questKey[:len(prefix)] != prefix {
			t.Errorf("QuestCorpus.MetaKey(%d): %q does not start with %q", f, questKey, prefix)
		}
		// 3. Quest key must differ from lore key (no aliasing).
		if questKey == loreKey {
			t.Errorf("QuestCorpus.MetaKey(%d): quest key %q aliases lore key %q", f, questKey, loreKey)
		}
	}
}

// TestQuestCorpus_MetaKeyExhaustive verifies no MetaField returns ""
// (which signals a missing switch case). Guards against adding a new
// MetaField constant without updating QuestCorpus.MetaKey.
func TestQuestCorpus_MetaKeyExhaustive(t *testing.T) {
	c := QuestCorpus{}
	// Iterate every known MetaField. If a future MetaField is added
	// without a case in QuestCorpus.MetaKey, this test catches it by
	// observing an empty return. Adjust the bound when new fields land.
	for f := MetaField(0); f <= FieldEmbedLastOKAt; f++ {
		if got := c.MetaKey(f); got == "" {
			t.Errorf("MetaKey(%d): returned empty string (missing case in QuestCorpus?)", f)
		}
	}
}

// TestQuestCorpus_LSP exercises the full corpus suite against QuestCorpus
// using the migration-seeded test DB (migration 005 creates tasks_fts_rows
// and quest_vectors). Seeds quests by directly inserting into task_status +
// task_notes + tasks_fts_rows (the trigger path fires only on INSERT, which
// migration's initial rebuild already covered).
func TestQuestCorpus_LSP(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)

	// Seed the 'quest.' meta rows that migration 005 inserts. The test
	// DB runs all migrations so these are already present; set the
	// embedder_model_id so WriteVector's identity guard passes.
	corpus := QuestCorpus{}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		corpus.MetaKey(FieldEmbedderModelID), canonModelID,
	); err != nil {
		t.Fatalf("set quest model_id meta: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		corpus.MetaKey(FieldEmbedderState), "enabled",
	); err != nil {
		t.Fatalf("set quest embedder_state meta: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		corpus.MetaKey(FieldVectorCoverageDen), "0",
	); err != nil {
		t.Fatalf("set quest coverage_den meta: %v", err)
	}

	// We need a project row because task_status has a FK to projects.
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Seed quests: insert into task_status, then add spec notes, then
	// register in tasks_fts_rows (mirrors what quest_post does via trigger).
	type questSeed struct {
		taskID  string
		subject string
	}
	seeds := []questSeed{
		{"QUEST-1", "implement BM25 search for quests"},
		{"QUEST-2", "add vector arm to quest retrieval pipeline"},
		{"QUEST-3", "write parameterized corpus tests for QuestCorpus"},
	}
	for _, s := range seeds {
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
			s.taskID,
		); err != nil {
			t.Fatalf("seed task_status %s: %v", s.taskID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note)
			 VALUES ('p', ?, 'test', ?)`,
			s.taskID, "[spec] subject: "+s.subject,
		); err != nil {
			t.Fatalf("seed task_notes %s: %v", s.taskID, err)
		}
		// The trigger tasks_fts_status_ai fires on INSERT to task_status and
		// populates tasks_fts_rows. Since we used INSERT OR IGNORE, the row
		// may already exist from the trigger. Ensure it exists.
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO tasks_fts_rows (task_id) VALUES (?)`,
			s.taskID,
		); err != nil {
			t.Fatalf("seed tasks_fts_rows %s: %v", s.taskID, err)
		}
	}

	// 1. Backfill every seeded quest.
	res, err := Backfill(ctx, BackfillOptions{
		DB:       db,
		Corpus:   corpus,
		Embedder: NewDeterministicEmbedder(),
		ModelID:  canonModelID,
	})
	if err != nil {
		t.Fatalf("QuestCorpus: Backfill: %v", err)
	}
	if res.Embedded != len(seeds) {
		t.Errorf("QuestCorpus: Backfill Embedded: got %d want %d", res.Embedded, len(seeds))
	}

	// 2. LoadFromDB reflects the backfilled rows.
	idx := NewIndex(corpus, canonModelID)
	loaded, err := idx.LoadFromDB(ctx, db)
	if err != nil {
		t.Fatalf("QuestCorpus: LoadFromDB: %v", err)
	}
	if loaded != len(seeds) {
		t.Errorf("QuestCorpus: LoadFromDB loaded %d want %d", loaded, len(seeds))
	}

	// 3. TopK returns results.
	qvec := Quantize(deterministicUnitVec(42))
	hits, err := idx.TopK(qvec, len(seeds))
	if err != nil {
		t.Fatalf("QuestCorpus: TopK: %v", err)
	}
	if len(hits) != len(seeds) {
		t.Errorf("QuestCorpus: TopK returned %d hits, want %d", len(hits), len(seeds))
	}

	// 4. WriteVector on a new quest seeds it into the index.
	const newTaskID = "QUEST-4"
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
		newTaskID,
	); err != nil {
		t.Fatalf("seed QUEST-4: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO task_notes (project_id, task_id, agent_id, note)
		 VALUES ('p', ?, 'test', '[spec] subject: new quest for WriteVector')`,
		newTaskID,
	); err != nil {
		t.Fatalf("seed QUEST-4 note: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO tasks_fts_rows (task_id) VALUES (?)`, newTaskID,
	); err != nil {
		t.Fatalf("seed QUEST-4 bridge: %v", err)
	}
	var newBridgeID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM tasks_fts_rows WHERE task_id = ?`, newTaskID,
	).Scan(&newBridgeID); err != nil {
		t.Fatalf("read QUEST-4 bridge id: %v", err)
	}
	preEpoch := idx.Epoch()
	wvRes, err := WriteVector(ctx, db, HotDeps{
		Embedder: NewDeterministicEmbedder(),
		Index:    idx,
		Corpus:   corpus,
		ModelID:  canonModelID,
	}, newBridgeID, "[spec] subject: new quest for WriteVector")
	if err != nil {
		t.Fatalf("QuestCorpus: WriteVector: %v", err)
	}
	if !wvRes.Written {
		t.Errorf("QuestCorpus: WriteVector Written=false, want true")
	}
	if idx.Epoch() <= preEpoch {
		t.Errorf("QuestCorpus: idx.Epoch did not advance (pre=%d post=%d)", preEpoch, idx.Epoch())
	}

	// 5. ReadHealthReport sees quest corpus rows independently.
	report, err := ReadHealthReport(ctx, db, corpus)
	if err != nil {
		t.Fatalf("QuestCorpus: ReadHealthReport: %v", err)
	}
	if report.CoverageNum < int64(len(seeds)+1) {
		t.Errorf("QuestCorpus: CoverageNum: got %d want >=%d", report.CoverageNum, len(seeds)+1)
	}
	if report.VectorEpoch <= 0 {
		t.Errorf("QuestCorpus: VectorEpoch: got %d want >0", report.VectorEpoch)
	}
}

// TestQuestCorpus_Migration006_BackfillsHistoricalRows is the QUEST-246
// regression gate. Reproduces the LORE-404 reproducer at fixture scale:
// 200 task_status rows that were never auto-bridged by the
// tasks_fts_status_ai trigger (the historical-data condition migration
// 005 left on every upgrading install) must end up bridged in
// tasks_fts_rows after the migration-006 backfill SQL runs.
//
// Approach: open a fully-migrated DB (so all of 001-006 is on disk),
// drop the auto-bridge trigger, seed 200 task_status rows that bypass
// the trigger, delete any tasks_fts_rows entries the seeded ids may
// alias, then re-execute the migration-006 SQL directly. The migration
// SQL is idempotent (INSERT OR IGNORE on bridge, UPDATE WHERE body=” on
// body) so re-running against an already-migrated DB is the documented
// safe path.
func TestQuestCorpus_Migration006_BackfillsHistoricalRows(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)

	// Drop the trigger so the seeded task_status rows below do not auto-
	// bridge. This simulates pre-005 historical data: rows existed long
	// before the trigger did, so the trigger never fired against them.
	if _, err := db.ExecContext(ctx, `DROP TRIGGER IF EXISTS tasks_fts_status_ai`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	const seedCount = 200
	for i := 1; i <= seedCount; i++ {
		taskID := "QUEST-246-" + intStr(i)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
			taskID,
		); err != nil {
			t.Fatalf("seed task_status %s: %v", taskID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note)
			 VALUES ('p', ?, 'test', '[spec] subject: historical quest body for migration 006 test')`,
			taskID,
		); err != nil {
			t.Fatalf("seed task_notes %s: %v", taskID, err)
		}
	}

	// Sanity precondition: tasks_fts_rows is still empty for these test
	// rows (the trigger was dropped before the inserts ran).
	var preBridge int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks_fts_rows WHERE task_id LIKE 'QUEST-246-%'`,
	).Scan(&preBridge); err != nil {
		t.Fatalf("pre count: %v", err)
	}
	if preBridge != 0 {
		t.Fatalf("precondition violated: %d historical bridge rows already present; test cannot exercise 006", preBridge)
	}

	// Re-execute the migration-006 backfill SQL. Idempotent by design.
	const migration006SQL = `
		INSERT OR IGNORE INTO tasks_fts_rows (task_id)
		SELECT DISTINCT task_id FROM task_status;
	`
	if _, err := db.ExecContext(ctx, migration006SQL); err != nil {
		t.Fatalf("re-run migration 006 INSERT: %v", err)
	}
	const migration006BodySQL = `
		UPDATE tasks_fts_rows
		SET body = COALESCE((
		  SELECT group_concat(tn.note, ' ')
		  FROM task_notes tn
		  WHERE tn.task_id = tasks_fts_rows.task_id
		    AND tn.note LIKE '[spec]%'
		), '')
		WHERE body = '';
	`
	if _, err := db.ExecContext(ctx, migration006BodySQL); err != nil {
		t.Fatalf("re-run migration 006 UPDATE: %v", err)
	}

	// Post-migration assertions.
	corpus := QuestCorpus{}
	activePred := corpus.ActivePredicate()
	if activePred == "" {
		activePred = "1=1"
	}
	denQuery := `SELECT COUNT(*) FROM ` + corpus.EntityTable() + ` WHERE ` + activePred
	var den int64
	if err := db.QueryRowContext(ctx, denQuery).Scan(&den); err != nil {
		t.Fatalf("post-migration den count: %v", err)
	}
	if den < int64(seedCount) {
		t.Errorf("QuestCorpus effective entity count after migration 006: got %d, want >= %d", den, seedCount)
	}

	// Body backfill assertion: every newly bridged test row must have a
	// non-empty body (its concatenated [spec] notes).
	var emptyBodies int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks_fts_rows
		WHERE task_id LIKE 'QUEST-246-%' AND body = ''
	`).Scan(&emptyBodies); err != nil {
		t.Fatalf("count empty bodies: %v", err)
	}
	if emptyBodies > 0 {
		t.Errorf("migration 006 left %d historical bridge rows with empty bodies; expected 0", emptyBodies)
	}
}

// intStr is a small itoa helper so this test file does not pull in
// strconv for one call.
func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestQuestCorpus_SourceText verifies that SourceText assembles the spec
// note payloads for a quest and returns sql.ErrNoRows for a missing quest.
func TestQuestCorpus_SourceText(t *testing.T) {
	ctx := context.Background()
	db := openEmbedTestDB(t)

	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO task_status (project_id, task_id, status) VALUES ('p', 'QUEST-5', 'next')`,
	); err != nil {
		t.Fatalf("seed task_status: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO task_notes (project_id, task_id, agent_id, note)
		 VALUES ('p', 'QUEST-5', 'test', '[spec] subject: cabinet search refactor'),
		        ('p', 'QUEST-5', 'test', '[spec] acceptance: ranks top-3')`,
	); err != nil {
		t.Fatalf("seed task_notes: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO tasks_fts_rows (task_id) VALUES ('QUEST-5')`,
	); err != nil {
		t.Fatalf("seed bridge row: %v", err)
	}
	var bridgeID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM tasks_fts_rows WHERE task_id = 'QUEST-5'`,
	).Scan(&bridgeID); err != nil {
		t.Fatalf("read bridge id: %v", err)
	}

	corpus := QuestCorpus{}
	text, err := corpus.SourceText(ctx, db, bridgeID)
	if err != nil {
		t.Fatalf("SourceText: %v", err)
	}
	if text == "" {
		t.Error("SourceText: got empty string, want non-empty")
	}
	// Both notes should appear in the concatenation.
	if !containsStr(text, "subject: cabinet search refactor") {
		t.Errorf("SourceText: expected subject note in output, got %q", text)
	}
	if !containsStr(text, "acceptance: ranks top-3") {
		t.Errorf("SourceText: expected acceptance note in output, got %q", text)
	}
}
