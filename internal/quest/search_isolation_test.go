package quest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

func registerSearchProject(t *testing.T, db *sql.DB, pid string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO projects(id,path) VALUES (?,?)`, pid, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func searchBridgeID(t *testing.T, db *sql.DB, pid, taskID string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(), `SELECT id FROM tasks_fts_rows WHERE project_id=? AND task_id=?`, pid, taskID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestQuestSearch_ProjectCollisionAndCandidateScope(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	registerSearchProject(t, db, "demo")
	demo := mustPost(t, db, "demo", PostParams{Subject: "todo list json array"})
	local := mustPost(t, db, pid, PostParams{Subject: "repair unrelated network timeout"})
	if demo.ID != local.ID {
		t.Fatalf("fixture requires same-number collision: %s != %s", demo.ID, local.ID)
	}
	out, err := RunQuestSearchForProject(ctx, db, "todo list json array", 10, pid, nil)
	if err != nil || len(out.Results) != 0 {
		t.Fatalf("other project's spec contributed relevance: results=%+v err=%v", out.Results, err)
	}
	out, err = RunQuestSearchForProject(ctx, db, "todo list json array", 10, "demo", nil)
	if err != nil || len(out.Results) != 1 || out.Results[0].Subject != demo.Subject {
		t.Fatalf("project override: results=%+v err=%v", out.Results, err)
	}
	for i := 0; i < questRRFTopK+10; i++ {
		mustPost(t, db, "demo", PostParams{Subject: "needle needle needle"})
	}
	desired := mustPost(t, db, pid, PostParams{Subject: "needle with additional less relevant words"})
	// Completed quests must stay searchable.
	if _, err := db.ExecContext(ctx, `UPDATE task_status SET status='done' WHERE project_id=? AND task_id=?`, pid, desired.ID); err != nil {
		t.Fatal(err)
	}
	out, err = RunQuestSearchForProject(ctx, db, "needle", 1, pid, nil)
	if err != nil || len(out.Results) != 1 || out.Results[0].QuestID != desired.ID || out.Results[0].Status != "done" {
		t.Fatalf("project filtering occurred after candidate limit: results=%+v err=%v", out.Results, err)
	}
}

// fixedQuestEmbedder keeps vector ranking controllable; real SQLite still
// owns the bridge, content hashes, stored vectors, and project eligibility.
type fixedQuestEmbedder struct {
	before func()
}

func (e fixedQuestEmbedder) Dimension() int { return embed.Dim }
func (e fixedQuestEmbedder) Embed(context.Context, string) ([]float32, error) {
	if e.before != nil {
		e.before()
	}
	v := make([]float32, embed.Dim)
	v[0] = 1
	return v, nil
}

func seedSearchVectors(t *testing.T, db *sql.DB, wantedProject string) *QuestEmbedDeps {
	t.Helper()
	ctx := context.Background()
	const modelID = "test-quest-model"
	upsertQuestMeta(t, db, "quest.embedder_model_id", modelID)
	upsertQuestMeta(t, db, "quest.embedder_state", "enabled")
	rows, err := db.QueryContext(ctx, `SELECT id, project_id FROM tasks_fts_rows WHERE body != '' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id  int64
		pid string
	}
	var entries []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.pid); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	for _, r := range entries {
		text, err := (embed.QuestCorpus{}).SourceText(ctx, db, r.id)
		if err != nil {
			t.Fatal(err)
		}
		v := make([]float32, embed.Dim)
		v[0] = 1 // Other projects outrank every local vector.
		if r.pid == wantedProject {
			v[0], v[1] = 0.5, 0.866
		}
		if err := embed.InsertVectorRow(ctx, db, embed.QuestCorpus{}, embed.PendingEntry{ID: r.id, Summary: text}, v, modelID); err != nil {
			t.Fatal(err)
		}
	}
	idx := embed.NewIndex(embed.QuestCorpus{}, modelID)
	if _, err := idx.LoadFromDB(ctx, db); err != nil {
		t.Fatal(err)
	}
	return &QuestEmbedDeps{Embedder: fixedQuestEmbedder{}, Index: idx, ModelID: modelID}
}

func TestQuestSearch_VectorScopeBeforeLimit(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	registerSearchProject(t, db, "other")
	for i := 0; i < questRRFTopK+10; i++ {
		mustPost(t, db, "other", PostParams{Subject: fmt.Sprintf("foreign vector number %d", i)})
	}
	wanted := mustPost(t, db, pid, PostParams{Subject: "local vector answer"})
	deps := seedSearchVectors(t, db, pid)
	// Coverage counters are deliberately wrong: the gate must use current,
	// project-qualified source and vector data rather than the cached meta.
	upsertQuestMeta(t, db, "quest.vector_coverage_num", "0")
	upsertQuestMeta(t, db, "quest.vector_coverage_den", "999")
	out, err := RunQuestSearchForProject(ctx, db, "semantically", 1, pid, deps)
	if err != nil || out.Arm != "rrf" || out.Coverage != 1 || len(out.Results) != 1 || out.Results[0].Subject != wanted.Subject {
		t.Fatalf("vector project scope before limit: out=%+v err=%v", out, err)
	}
	// A foreign bridge ID can never be hydrated as the same-number local task.
	foreign := searchBridgeID(t, db, "other", wanted.ID)
	results, err := hydrateQuestResults(ctx, db, []int64{foreign, searchBridgeID(t, db, pid, wanted.ID)}, 1, pid)
	if err != nil || len(results) != 1 || results[0].Subject != wanted.Subject {
		t.Fatalf("project-qualified hydration: results=%+v err=%v", results, err)
	}
}

func TestQuestSearch_RefillsAfterConcurrentDelete(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	var tasks []string
	for i := 0; i < questRRFTopK+5; i++ {
		q := mustPost(t, db, pid, PostParams{Subject: "refillcandidate"})
		tasks = append(tasks, q.ID)
	}
	deps := seedSearchVectors(t, db, pid)
	deleted := false
	deps.Embedder = fixedQuestEmbedder{before: func() {
		if deleted {
			return
		}
		deleted = true
		// Delete the entire first candidate window after scoring eligibility
		// was read, forcing a second retrieval pass rather than a short result.
		for _, id := range tasks[:questRRFTopK] {
			if _, err := db.ExecContext(ctx, `DELETE FROM task_status WHERE project_id=? AND task_id=?`, pid, id); err != nil {
				t.Fatal(err)
			}
		}
	}}
	out, err := RunQuestSearchForProject(ctx, db, "refillcandidate", 5, pid, deps)
	if err != nil || len(out.Results) != 5 {
		t.Fatalf("result refill: out=%+v err=%v", out, err)
	}
	for _, r := range out.Results {
		for _, gone := range tasks[:questRRFTopK] {
			if r.QuestID == gone {
				t.Fatalf("deleted candidate survived: %s", gone)
			}
		}
	}
}

func TestQuestSearch_ProjectScopedLifecycle(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	registerSearchProject(t, db, "other")
	foreign := mustPost(t, db, "other", PostParams{Subject: "foreign original subject"})
	local := mustPost(t, db, pid, PostParams{Subject: "local original subject"})
	localID := searchBridgeID(t, db, pid, local.ID)
	foreignID := searchBridgeID(t, db, "other", foreign.ID)
	deps := seedSearchVectors(t, db, pid)
	if _, err := Update(ctx, db, pid, local.ID, UpdateParams{ReplaceAcceptance: []string{"replacementacceptance"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quest_vectors WHERE entry_id=?`, localID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("local spec update left stale vector: n=%d err=%v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quest_vectors WHERE entry_id=?`, foreignID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("local update invalidated other project's vector: n=%d err=%v", n, err)
	}
	body, err := (embed.QuestCorpus{}).SourceText(ctx, db, localID)
	canonical, canonicalErr := embed.QuestSourceText(ctx, db, pid, local.ID)
	if err != nil || canonicalErr != nil || body != canonical || !strings.Contains(body, "replacementacceptance") || strings.Contains(body, "foreign") {
		t.Fatalf("updated sources disagree: body=%q canonical=%q err=%v/%v", body, canonical, err, canonicalErr)
	}
	out, err := RunQuestSearchForProject(ctx, db, "replacementacceptance", 10, pid, deps)
	if err != nil || len(out.Results) != 1 || out.Results[0].QuestID != local.ID || out.Arm != "bm25" {
		t.Fatalf("updated spec searchable with stale vector excluded: out=%+v err=%v", out, err)
	}
	// Direct note edits/deletes must use the same project-scoped trigger path.
	if _, err := db.ExecContext(ctx, `UPDATE task_notes SET note='[spec] subject: editedsubject' WHERE project_id=? AND task_id=? AND note LIKE '[spec] subject:%'`, pid, local.ID); err != nil {
		t.Fatal(err)
	}
	out, err = RunQuestSearchForProject(ctx, db, "editedsubject", 1, pid, nil)
	if err != nil || len(out.Results) != 1 || out.Results[0].Subject != "editedsubject" {
		t.Fatalf("note edit failed to refresh FTS: out=%+v err=%v", out, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM task_notes WHERE project_id=? AND task_id=? AND note LIKE '[spec-replace]%'`, pid, local.ID); err != nil {
		t.Fatal(err)
	}
	out, err = RunQuestSearchForProject(ctx, db, "replacementacceptance", 1, pid, nil)
	if err != nil || len(out.Results) != 0 {
		t.Fatalf("deleted note still indexed: out=%+v err=%v", out, err)
	}
	// Archive and restore into another project must produce a separate bridge.
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := Archive(ctx, db, pid, path); err != nil {
		t.Fatal(err)
	}
	registerSearchProject(t, db, "restored")
	if _, err := Restore(ctx, db, "restored", path); err != nil {
		t.Fatal(err)
	}
	out, err = RunQuestSearchForProject(ctx, db, "editedsubject", 1, "restored", nil)
	if err != nil || len(out.Results) != 1 || out.Results[0].Subject != "editedsubject" {
		t.Fatalf("restore did not construct project bridge: out=%+v err=%v", out, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM task_status WHERE project_id=? AND task_id=?`, pid, local.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := (embed.QuestCorpus{}).SourceText(ctx, db, localID); err != sql.ErrNoRows {
		t.Fatalf("deleted quest bridge still present: %v", err)
	}
	out, err = RunQuestSearchForProject(ctx, db, "foreign original", 1, "other", nil)
	if err != nil || len(out.Results) != 1 || out.Results[0].QuestID != foreign.ID {
		t.Fatalf("same-number peer damaged by deletion: out=%+v err=%v", out, err)
	}
}

func TestQuestSearch_PartialFreshVectorsAndDisabledFallback(t *testing.T) {
	db, pid := newTestDB(t)
	ctx := context.Background()
	lexical := mustPost(t, db, pid, PostParams{Subject: "partialkeyword lexical answer"})
	semantic := mustPost(t, db, pid, PostParams{Subject: "semantic answer"})
	deps := seedSearchVectors(t, db, pid)
	// The strongest lexical answer has no vector. Partial vector coverage
	// should remain usable without displacing that independent BM25 evidence.
	if _, err := db.ExecContext(ctx, `DELETE FROM quest_vectors WHERE entry_id=?`, searchBridgeID(t, db, pid, lexical.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE meta SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT) WHERE key='quest.vector_epoch'`); err != nil {
		t.Fatal(err)
	}
	out, err := RunQuestSearchForProject(ctx, db, "partialkeyword", 2, pid, deps)
	if err != nil || out.Arm != "rrf" || out.Coverage != 0.5 || len(out.Results) != 2 || out.Results[0].QuestID != lexical.ID || out.Results[1].QuestID != semantic.ID {
		t.Fatalf("partial fresh vector policy: out=%+v err=%v", out, err)
	}
	upsertQuestMeta(t, db, "quest.embedder_state", "disabled")
	out, err = RunQuestSearchForProject(ctx, db, "partialkeyword", 2, pid, deps)
	if err != nil || out.Arm != "bm25" || len(out.Results) != 1 || out.Results[0].QuestID != lexical.ID {
		t.Fatalf("disabled state ignored retained vector dependencies: out=%+v err=%v", out, err)
	}
}
