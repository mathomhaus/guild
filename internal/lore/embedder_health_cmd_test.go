package lore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/storage"
)

func TestEmbedderHealth_IncludesQuestCorpusSection(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lorePath := filepath.Join(tmp, "lore.db")
	questPath := filepath.Join(tmp, "quest.db")

	seedLoreHealthDB(t, lorePath, 8, 10)
	seedQuestHealthDB(t, questPath, 3, 5)

	deps := command.Deps{
		OpenDB: func(ctx context.Context) (*sql.DB, error) {
			return storage.Open(ctx, lorePath)
		},
		OpenQuestDB: func(ctx context.Context) (*sql.DB, error) {
			return storage.Open(ctx, questPath)
		},
		ResolveProj: func(_ context.Context, _ string) (string, error) {
			return "testproj", nil
		},
	}

	out, err := EmbedderHealthCommand.Handler(ctx, deps, EmbedderHealthInput{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out.LoreReport == nil {
		t.Fatal("expected lore report")
	}
	if out.LoreReport.CoverageNum != 8 || out.LoreReport.CoverageDen != 10 {
		t.Fatalf("lore coverage = %d/%d, want 8/10", out.LoreReport.CoverageNum, out.LoreReport.CoverageDen)
	}
	if out.QuestReport == nil {
		t.Fatal("expected quest report")
	}
	if out.QuestReport.CoverageNum != 3 || out.QuestReport.CoverageDen != 5 {
		t.Fatalf("quest coverage = %d/%d, want 3/5", out.QuestReport.CoverageNum, out.QuestReport.CoverageDen)
	}

	text := formatEmbedderHealth(command.CLISink{NoEmoji: true}, out)
	if !strings.Contains(text, "lore corpus embedder section") {
		t.Fatalf("output missing lore section:\n%s", text)
	}
	if !strings.Contains(text, "quest corpus embedder section") {
		t.Fatalf("output missing quest section:\n%s", text)
	}
	if !strings.Contains(text, "coverage:        8/10") {
		t.Fatalf("output missing lore coverage:\n%s", text)
	}
	if !strings.Contains(text, "coverage:        3/5") {
		t.Fatalf("output missing quest coverage:\n%s", text)
	}
}

func TestEmbedderHealth_EmptyQuestCorpusRendersZeroCoverage(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lorePath := filepath.Join(tmp, "lore.db")

	seedLoreHealthDB(t, lorePath, 0, 0)

	deps := command.Deps{
		OpenDB: func(ctx context.Context) (*sql.DB, error) {
			return storage.Open(ctx, lorePath)
		},
		ResolveProj: func(_ context.Context, _ string) (string, error) {
			return "testproj", nil
		},
	}

	out, err := EmbedderHealthCommand.Handler(ctx, deps, EmbedderHealthInput{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out.QuestReport == nil {
		t.Fatal("expected quest report placeholder")
	}
	if out.QuestReport.CoverageNum != 0 || out.QuestReport.CoverageDen != 0 {
		t.Fatalf("quest coverage = %d/%d, want 0/0", out.QuestReport.CoverageNum, out.QuestReport.CoverageDen)
	}

	text := formatEmbedderHealth(command.CLISink{NoEmoji: true}, out)
	if !strings.Contains(text, "quest corpus embedder section") {
		t.Fatalf("output missing quest section:\n%s", text)
	}
	if !strings.Contains(text, "coverage:        0/0 (0.0%)") {
		t.Fatalf("output missing zero quest coverage:\n%s", text)
	}
}

func seedLoreHealthDB(t *testing.T, path string, covNum, covDen int64) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("migrate lore: %v", err)
	}
	rows := []struct{ k, v string }{
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "abc123"},
		{"embedder_runtime_version", "onnxruntime-1.23.x"},
		{"embedder_dim", "384"},
		{"embedder_state", "enabled"},
		{"vector_epoch", "1"},
		{"vector_coverage_num", fmt.Sprintf("%d", covNum)},
		{"vector_coverage_den", fmt.Sprintf("%d", covDen)},
		{"embed_error_count", "0"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, r.k, r.v,
		); err != nil {
			t.Fatalf("seed lore meta %s: %v", r.k, err)
		}
	}
}

func seedQuestHealthDB(t *testing.T, path string, covNum, covDen int64) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		t.Fatalf("migrate quest: %v", err)
	}
	rows := []struct{ k, v string }{
		{"quest.embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"quest.embedder_tokenizer_hash", "abc123"},
		{"quest.embedder_runtime_version", "onnxruntime-1.23.x"},
		{"quest.embedder_dim", "384"},
		{"quest.embedder_state", "enabled"},
		{"quest.vector_epoch", "2"},
		{"quest.vector_coverage_num", fmt.Sprintf("%d", covNum)},
		{"quest.vector_coverage_den", fmt.Sprintf("%d", covDen)},
		{"quest.embed_error_count", "0"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, r.k, r.v,
		); err != nil {
			t.Fatalf("seed quest meta %s: %v", r.k, err)
		}
	}
}
