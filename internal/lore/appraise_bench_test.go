package lore

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

func benchmarkLoreDB(b *testing.B) *sql.DB {
	b.Helper()
	db, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if err := storage.MigrateTo(context.Background(), db, "benchmark", io.Discard); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects(id,path)VALUES('bench','/bench')`); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if _, err := db.Exec(`INSERT INTO entries(project_id,topic,kind,title,summary,status)VALUES('bench','bench','decision',?,'summary','current')`, fmt.Sprintf("Durable cache architecture and request admission finding %d", i)); err != nil {
			b.Fatal(err)
		}
	}
	return db
}

func BenchmarkEligibleEntryIDs(b *testing.B) {
	db := benchmarkLoreDB(b)
	p := &AppraiseParams{Query: "Durable cache architecture and request admission finding 999", AllProjects: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := eligibleEntryIDs(context.Background(), db, p, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSortAppraiseResults(b *testing.B) {
	corpus := make([]AppraiseResult, 120)
	for i := range corpus {
		corpus[i] = AppraiseResult{Entry: &Entry{ID: int64(i + 1), Title: fmt.Sprintf("Durable evidence title %d", i)}, Score: float64(i)}
	}
	cfg := DefaultScoring()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results := append([]AppraiseResult(nil), corpus...)
		sortAppraiseResults(results, "Durable evidence title 37", cfg)
	}
}

func BenchmarkAppraiseLexical(b *testing.B) {
	db := benchmarkLoreDB(b)
	p := AppraiseParams{Query: "Durable cache architecture and request admission finding 999", AllProjects: true, Limit: 10, Now: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := Appraise(context.Background(), db, p)
		if err != nil {
			b.Fatal(err)
		}
		if len(out.Results) != 10 {
			b.Fatalf("unexpected result count %d", len(out.Results))
		}
	}
}
