package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"testing"
	"time"
)

// These vectors encode test geometry, not natural-language relevance.
type fixedQueryEmbedder struct{ fail bool }

func (e fixedQueryEmbedder) Embed(context.Context, string) ([]float32, error) {
	if e.fail {
		return nil, errors.New("test encode failure")
	}
	v := make([]float32, embed.Dim)
	v[0] = 1
	return v, nil
}
func (fixedQueryEmbedder) Dimension() int { return embed.Dim }

func seedGeometryVector(t *testing.T, db *sql.DB, id int64, summary string, first float32) {
	t.Helper()
	v := make([]float32, embed.Dim)
	v[0] = first
	v[1] = 1 - first
	// WriteVector supplies the genuine source hash and lifecycle metadata.
	_, err := embed.WriteVector(context.Background(), db, embed.HotDeps{Embedder: geometryEmbedder{v}, ModelID: "geometry"}, id, summary)
	if err != nil {
		t.Fatal(err)
	}
}

type geometryEmbedder struct{ v []float32 }

func (e geometryEmbedder) Embed(context.Context, string) ([]float32, error) { return e.v, nil }
func (geometryEmbedder) Dimension() int                                     { return embed.Dim }

func geometryDeps(t *testing.T, db *sql.DB) *EmbedDeps {
	t.Helper()
	idx := embed.NewIndex(embed.LoreCorpus{}, "geometry")
	if _, err := idx.LoadFromDB(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return &EmbedDeps{Embedder: fixedQueryEmbedder{}, ModelID: "geometry", Index: idx}
}

func setGeometryMeta(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, kv := range [][2]string{{"embedder_model_id", "geometry"}, {"embedder_state", "enabled"}, {"vector_coverage_num", "0"}, {"vector_coverage_den", "99999"}} {
		if _, err := db.Exec(`INSERT OR REPLACE INTO meta(key,value)VALUES(?,?)`, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppraiseFiltersBeforeVectorLimit(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()
	setGeometryMeta(t, db)
	corpus := []fixtureEntry{{"small", "decision", "Unusual local evidence", "opaque relevant summary", ""}}
	for i := 0; i < 80; i++ {
		corpus = append(corpus, fixtureEntry{"large", "decision", fmt.Sprintf("Outside %d", i), "unrelated external text", ""})
	}
	// Excluded status and old rows in the same project must not take slots.
	corpus = append(corpus, fixtureEntry{"small", "decision", "Old same project", "old summary", ""}, fixtureEntry{"small", "decision", "Sealed same project", "sealed summary", ""})
	ids := seedCorpus(t, ctx, db, corpus)
	for i, e := range corpus {
		first := float32(1)
		if i == 0 {
			first = 0.5
		}
		seedGeometryVector(t, db, ids[i], e.Summary, first)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`UPDATE entries SET created_at=?`, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE entries SET created_at=? WHERE id=?`, now.AddDate(0, 0, -90).Format(time.RFC3339), ids[len(ids)-2]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE entries SET status='sealed' WHERE id=?`, ids[len(ids)-1]); err != nil {
		t.Fatal(err)
	}
	deps := geometryDeps(t, db)
	out, err := Appraise(ctx, db, AppraiseParams{Query: "words absent from all texts", Project: "small", Now: now, Since: 7 * 24 * time.Hour, Limit: 1, Embed: deps})
	if err != nil {
		t.Fatal(err)
	}
	if out.RetrievalMode != "hybrid" || len(out.Results) != 1 || out.Results[0].Entry.ID != ids[0] {
		t.Fatalf("small scope starved: %+v", out)
	}
	if !out.Results[0].SemanticMatch || out.Results[0].LexicalMatch {
		t.Fatalf("vector-only rescue did not execute: %+v", out.Results[0])
	}
}

func TestAppraisePartialIndexProtectsMissingExactTitle(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()
	setGeometryMeta(t, db)
	corpus := []fixtureEntry{{"small", "decision", "  Exact   lookup title  ", "summary without title words", ""}}
	for i := 0; i < 80; i++ {
		corpus = append(corpus, fixtureEntry{"small", "decision", fmt.Sprintf("lookup evidence %d", i), "exact lookup title repeated title lookup exact", ""})
	}
	ids := seedCorpus(t, ctx, db, corpus)
	for i := 1; i < len(corpus); i++ {
		seedGeometryVector(t, db, ids[i], corpus[i].Summary, 1)
	}
	deps := geometryDeps(t, db)
	for _, global := range []bool{true, false} {
		out, err := Appraise(ctx, db, AppraiseParams{Query: "exact lookup title", Project: "small", AllProjects: global, Limit: 1, Embed: deps})
		if err != nil {
			t.Fatal(err)
		}
		if out.RetrievalMode != "hybrid" || len(out.Results) != 1 || out.Results[0].Entry.ID != ids[0] {
			t.Fatalf("missing-vector exact title disappeared (global=%v): %+v", global, out)
		}
	}
	deps.Embedder = fixedQueryEmbedder{fail: true}
	out, err := Appraise(ctx, db, AppraiseParams{Query: "exact lookup title", Project: "small", Limit: 1, Embed: deps})
	if err != nil {
		t.Fatal(err)
	}
	if out.RetrievalMode != "lexical" || len(out.Results) != 1 || out.Results[0].Entry.ID != ids[0] {
		t.Fatal("encode failure did not preserve lexical fallback")
	}
	deps.Embedder = embed.NewNullEmbedder()
	out, err = Appraise(ctx, db, AppraiseParams{Query: "exact lookup title", AllProjects: true, Limit: 1, Embed: deps})
	if err != nil {
		t.Fatal(err)
	}
	if out.RetrievalMode != "lexical" || out.Results[0].Entry.ID != ids[0] {
		t.Fatal("disabled embedder did not fall back")
	}
}

func TestLexicalEvidenceDoesNotSaturateOrDecayByDefault(t *testing.T) {
	cfg := DefaultScoring()
	old := &Entry{Title: "Durable evidence", CreatedAt: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)}
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := &Entry{Title: "Weak recent mention", CreatedAt: now}
	for _, scores := range [][2]float64{{-80, -50}, {-1e-5, -1e-6}} {
		if Score(old, "query", scores[0], cfg, now) <= Score(recent, "query", scores[1], cfg, now) {
			t.Fatalf("durable evidence displaced at BM25 scores %v", scores)
		}
	}
}

func TestAppraiseHonorsOperatorDisableAfterWiring(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()
	setGeometryMeta(t, db)
	corpus := []fixtureEntry{{"small", "decision", "Direct lookup", "source evidence", ""}}
	ids := seedCorpus(t, ctx, db, corpus)
	seedGeometryVector(t, db, ids[0], corpus[0].Summary, 1)
	deps := geometryDeps(t, db)
	if _, err := db.Exec(`UPDATE meta SET value='disabled' WHERE key='embedder_state'`); err != nil {
		t.Fatal(err)
	}
	out, err := Appraise(ctx, db, AppraiseParams{Query: "Direct lookup", Project: "small", Embed: deps})
	if err != nil {
		t.Fatal(err)
	}
	if out.RetrievalMode != "lexical" || len(out.Results) != 1 {
		t.Fatalf("operator disabled semantic retrieval but wired runtime used it: %+v", out)
	}
}
