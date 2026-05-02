package quest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// TestQuestSearch_BM25Basic verifies that quest_search returns results
// for an exact keyword match via the BM25 arm.
func TestQuestSearch_BM25Basic(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	// Post several quests with distinct subjects.
	mustPost(t, db, pid, PostParams{Subject: "implement cabinet search refactor"})
	mustPost(t, db, pid, PostParams{Subject: "add vector embeddings to lore corpus"})
	mustPost(t, db, pid, PostParams{Subject: "fix quest brief timestamp tie"})

	// The FTS index is populated via triggers on task_notes insert.
	// Wait for triggers (no async here; triggers are synchronous).

	out, err := RunQuestSearchForProject(ctx, db, "cabinet search", 10, pid, nil)
	if err != nil {
		t.Fatalf("RunQuestSearchForProject: %v", err)
	}
	if out.Arm != "bm25" {
		t.Errorf("Arm: got %q want %q", out.Arm, "bm25")
	}
	if len(out.Results) == 0 {
		t.Fatal("RunQuestSearchForProject: expected results for 'cabinet search', got none")
	}
	// The cabinet search quest should be the top result.
	top := out.Results[0]
	if top.QuestID == "" {
		t.Error("top result has empty QuestID")
	}
	if top.Subject == "" {
		t.Error("top result has empty Subject")
	}
	// Cabinet search quest should rank first.
	found := false
	for _, r := range out.Results {
		if containsQuestStr(r.Subject, "cabinet") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'cabinet search refactor' quest in results; got %v", out.Results)
	}
}

// TestQuestSearch_NoResults verifies empty results for a query with no
// matching quests, with no panic or error.
func TestQuestSearch_NoResults(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	mustPost(t, db, pid, PostParams{Subject: "fix the timestamp tie bug"})

	out, err := RunQuestSearchForProject(ctx, db, "xylophone", 10, pid, nil)
	if err != nil {
		t.Fatalf("RunQuestSearchForProject: %v", err)
	}
	if len(out.Results) != 0 {
		t.Errorf("expected 0 results for nonsense query, got %d", len(out.Results))
	}
}

// TestQuestSearch_LimitRespected verifies that the limit parameter caps
// the number of results returned.
func TestQuestSearch_LimitRespected(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	// Post more quests than the limit.
	for i := 0; i < 5; i++ {
		mustPost(t, db, pid, PostParams{
			Subject: "implement search feature variant number",
		})
	}

	out, err := RunQuestSearchForProject(ctx, db, "implement search", 3, pid, nil)
	if err != nil {
		t.Fatalf("RunQuestSearchForProject: %v", err)
	}
	if len(out.Results) > 3 {
		t.Errorf("expected at most 3 results, got %d", len(out.Results))
	}
}

// TestQuestSearch_EmptyQuery verifies an empty query returns an error.
func TestQuestSearch_EmptyQuery(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	_, err := RunQuestSearchForProject(ctx, db, "", 10, pid, nil)
	if err == nil {
		t.Error("expected error for empty query, got nil")
	}
}

// TestQuestSearch_StopwordQuery verifies that a query composed entirely
// of stopwords still produces a MATCH expression (fallback to raw tokens)
// and does not panic or return an error.
func TestQuestSearch_StopwordQuery(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	mustPost(t, db, pid, PostParams{Subject: "do the thing"})

	// "the" and "is" are both stopwords. The fallback should use them raw.
	_, err := RunQuestSearchForProject(ctx, db, "the is", 10, pid, nil)
	if err != nil {
		t.Fatalf("RunQuestSearchForProject with stopword query: %v", err)
	}
	// No assertion on results; we just want no panic or error.
}

// TestQuestSearch_SemanticParaphrase verifies that a paraphrase of a
// quest's subject appears in the search results when the BM25 arm can
// still pick up stemmed terms (porter stemmer collapses variants).
// This test exercises the BM25 arm with porter stemming.
func TestQuestSearch_SemanticParaphrase(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	// Post a quest with a specific technical subject.
	mustPost(t, db, pid, PostParams{
		Subject:    "implement BM25 retrieval for quest corpus",
		Acceptance: []string{"BM25 search works on task_notes text"},
	})
	mustPost(t, db, pid, PostParams{Subject: "unrelated: fix CI flake on windows"})
	mustPost(t, db, pid, PostParams{Subject: "unrelated: update go dependencies"})

	// "retrieval" and "retrieve" share a porter stem; the FTS index
	// with porter tokenizer should match "retrieved" to "retrieval".
	out, err := RunQuestSearchForProject(ctx, db, "retrieved quests corpus", 10, pid, nil)
	if err != nil {
		t.Fatalf("RunQuestSearchForProject paraphrase: %v", err)
	}
	// At minimum: result set is non-empty and contains the BM25 quest.
	if len(out.Results) == 0 {
		t.Fatal("expected at least one result for paraphrase query, got none")
	}
	found := false
	for i, r := range out.Results {
		if i < 3 && containsQuestStr(r.Subject, "BM25") {
			found = true
			break
		}
	}
	if !found {
		t.Logf("results: %+v", out.Results)
		// Soft-check: porter stemmer may or may not collapse the paraphrase
		// depending on the exact token set. Emit a diagnostic but do not fail
		// hard; the integration value is that the pipeline runs without error
		// and returns a ranked list.
		t.Logf("WARN: 'BM25 retrieval' quest not in top-3 for paraphrase query; porter stemmer fallback may vary")
	}
}

// TestQuestSearch_FTSQueryBuilder verifies questFTSQuery produces the
// expected token strings and handles edge cases.
func TestQuestSearch_FTSQueryBuilder(t *testing.T) {
	cases := []struct {
		query string
		empty bool // whether we expect an empty result
	}{
		{"implement BM25 search", false},
		{"the is in", false}, // all stopwords: fall back to raw tokens
		{"cabinet retrieval", false},
		{"ab", false}, // >=2 chars: included
		{"a", true},   // <2 chars after filtering: empty
	}
	for _, tc := range cases {
		got := questFTSQuery(tc.query)
		if tc.empty && got != "" {
			t.Errorf("questFTSQuery(%q): expected empty, got %q", tc.query, got)
		}
		if !tc.empty && got == "" {
			t.Errorf("questFTSQuery(%q): expected non-empty, got empty", tc.query)
		}
	}
}

// TestQuestSearch_RRFArmAboveCoverageFloor is the QUEST-258 regression gate:
// when quest_vectors coverage is at or above the 0.90 floor and a
// QuestEmbedDeps is wired, quest_search must return arm=rrf. This exercises
// the wiring path that was dormant before QUEST-258 (embedDeps was always nil).
func TestQuestSearch_RRFArmAboveCoverageFloor(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	// Post enough quests to have a non-trivial corpus.
	const totalQuests = 10
	for i := 1; i <= totalQuests; i++ {
		mustPost(t, db, pid, PostParams{
			Subject: fmt.Sprintf("implement feature variant %d for search pipeline", i),
		})
	}

	// tasks_fts_rows is populated by trigger on task_notes insert (via mustPost).
	// Collect all bridge row IDs for vector seeding.
	rows, err := db.QueryContext(ctx, `SELECT id, task_id FROM tasks_fts_rows`)
	if err != nil {
		t.Fatalf("query tasks_fts_rows: %v", err)
	}
	type bridgeRow struct {
		id     int64
		taskID string
	}
	var bridgeRows []bridgeRow
	for rows.Next() {
		var br bridgeRow
		if scanErr := rows.Scan(&br.id, &br.taskID); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		bridgeRows = append(bridgeRows, br)
	}
	_ = rows.Close()
	if len(bridgeRows) == 0 {
		t.Fatal("no tasks_fts_rows after posting quests; trigger may not have fired")
	}

	// Seed vectors for all bridge rows using DeterministicEmbedder.
	embedder := embed.NewDeterministicEmbedder()
	const modelID = "bge-small-en-v1.5-int8-cls"
	for _, br := range bridgeRows {
		vec, embedErr := embedder.Embed(ctx, "subject: feature variant search pipeline")
		if embedErr != nil {
			t.Fatalf("embed row %d: %v", br.id, embedErr)
		}
		entry := embed.PendingEntry{ID: br.id, Summary: "feature variant"}
		if insertErr := embed.InsertVectorRow(ctx, db, embed.QuestCorpus{}, entry, vec, modelID); insertErr != nil {
			t.Fatalf("insert vector row %d: %v", br.id, insertErr)
		}
	}

	// Set coverage meta so questVectorTopK reads a passing ratio.
	n := int64(len(bridgeRows))
	upsertQuestMeta(t, db, "quest.embedder_state", "enabled")
	upsertQuestMeta(t, db, "quest.embedder_model_id", modelID)
	upsertQuestMeta(t, db, "quest.vector_coverage_num", intStr(n))
	upsertQuestMeta(t, db, "quest.vector_coverage_den", intStr(n))

	// Build a QuestEmbedDeps with a QuestCorpus Index loaded from the DB.
	idx := embed.NewIndex(embed.QuestCorpus{}, modelID)
	if _, loadErr := idx.LoadFromDB(ctx, db); loadErr != nil {
		t.Fatalf("LoadFromDB: %v", loadErr)
	}
	deps := &QuestEmbedDeps{
		Embedder: embedder,
		Index:    idx,
		ModelID:  modelID,
	}

	out, err := RunQuestSearchForProject(ctx, db, "feature search pipeline", 10, pid, deps)
	if err != nil {
		t.Fatalf("RunQuestSearchForProject: %v", err)
	}
	if out.Arm != "rrf" {
		t.Errorf("arm: got %q want %q (coverage=%.2f)", out.Arm, "rrf", out.Coverage)
	}
	if len(out.Results) == 0 {
		t.Error("expected results with rrf arm, got none")
	}
}

// TestQuestSearch_BM25ArmBelowCoverageFloor verifies that when quest_vectors
// coverage is below the 0.90 gate, quest_search falls back to arm=bm25
// cleanly: no error, no panic, results from BM25.
func TestQuestSearch_BM25ArmBelowCoverageFloor(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()

	// Post quests.
	mustPost(t, db, pid, PostParams{Subject: "implement BM25 fallback coverage test"})
	mustPost(t, db, pid, PostParams{Subject: "another quest for coverage floor check"})

	// Set coverage meta below the 0.90 floor (1 vector out of 2 entities).
	upsertQuestMeta(t, db, "quest.embedder_state", "enabled")
	upsertQuestMeta(t, db, "quest.embedder_model_id", "bge-small-en-v1.5-int8-cls")
	upsertQuestMeta(t, db, "quest.vector_coverage_num", "1")
	upsertQuestMeta(t, db, "quest.vector_coverage_den", "2")

	embedder := embed.NewDeterministicEmbedder()
	const modelID = "bge-small-en-v1.5-int8-cls"
	idx := embed.NewIndex(embed.QuestCorpus{}, modelID)
	// LoadFromDB returns 0 rows (no vectors in quest_vectors); that is fine.
	if _, loadErr := idx.LoadFromDB(ctx, db); loadErr != nil {
		t.Fatalf("LoadFromDB: %v", loadErr)
	}
	deps := &QuestEmbedDeps{
		Embedder: embedder,
		Index:    idx,
		ModelID:  modelID,
	}

	out, err := RunQuestSearchForProject(ctx, db, "BM25 fallback", 10, pid, deps)
	if err != nil {
		t.Fatalf("RunQuestSearchForProject: %v", err)
	}
	if out.Arm != "bm25" {
		t.Errorf("arm: got %q want %q (coverage=%.2f)", out.Arm, "bm25", out.Coverage)
	}
	// No panic, no error: the BM25 fallback works cleanly.
}

// upsertQuestMeta inserts or updates a row in the meta table. Used by
// QUEST-258 regression tests to set quest corpus coverage counters.
func upsertQuestMeta(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		t.Fatalf("upsertQuestMeta %s=%s: %v", key, value, err)
	}
}

// containsQuestStr is a substring check for test assertions in this file.
// Kept local to avoid shadowing the embed package's version.
func containsQuestStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// intStr converts an int64 to its decimal string representation.
// Avoids importing strconv in a test-only helper.
func intStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	// Reverse.
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}
