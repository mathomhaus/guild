package lore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/storage"
)

const (
	embedRebuildTestModelID = "test-model"
	embedRebuildFakeDim     = 384
)

type embedRebuildFakeEmbedder struct{}

func (embedRebuildFakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, embedRebuildFakeDim)
	seed := float32(len(text)%7 + 1)
	for i := range vec {
		vec[i] = seed + float32(i%5)/10
	}
	return vec, nil
}

func (embedRebuildFakeEmbedder) Dimension() int {
	return embedRebuildFakeDim
}

func TestEmbedRebuild_RefusesWhenEmbedderNotWired(t *testing.T) {
	ctx := context.Background()
	dbPath, db := newEmbedRebuildTestDB(t, "proj")
	enableEmbedRebuildMeta(t, db)
	entryIDs := seedEmbedRebuildEntries(t, ctx, db, "proj")
	insertEmbedRebuildVectors(t, ctx, db, entryIDs)

	before := countEmbedRebuildVectors(t, ctx, db)
	if before == 0 {
		t.Fatal("test setup did not write initial vectors")
	}

	deps := stubCommandDeps(nil)
	deps.OpenDB = func(ctx context.Context) (*sql.DB, error) {
		return storage.Open(ctx, dbPath)
	}
	deps.ResolveProj = func(_ context.Context, _ string) (string, error) {
		return "proj", nil
	}

	out, err := EmbedRebuildCommand.Handler(ctx, deps, EmbedRebuildInput{})
	if err != nil {
		t.Fatalf("embed-rebuild: %v", err)
	}
	if !out.Disabled {
		t.Fatalf("Disabled = false, want true")
	}
	if out.Reason == "" {
		t.Fatal("Reason is empty")
	}

	after := countEmbedRebuildVectors(t, ctx, db)
	if after != before {
		t.Fatalf("vector rows after refusal = %d, want %d", after, before)
	}
}

func TestEmbedRebuild_HappyPathWithWiredEmbedder(t *testing.T) {
	ctx := context.Background()
	dbPath, db := newEmbedRebuildTestDB(t, "proj")
	enableEmbedRebuildMeta(t, db)
	seedEmbedRebuildEntries(t, ctx, db, "proj")

	deps := stubCommandDeps(&EmbedDeps{
		Embedder: embedRebuildFakeEmbedder{},
		ModelID:  embedRebuildTestModelID,
	})
	deps.OpenDB = func(ctx context.Context) (*sql.DB, error) {
		return storage.Open(ctx, dbPath)
	}
	deps.ResolveProj = func(_ context.Context, _ string) (string, error) {
		return "proj", nil
	}

	out, err := EmbedRebuildCommand.Handler(ctx, deps, EmbedRebuildInput{})
	if err != nil {
		t.Fatalf("embed-rebuild: %v", err)
	}
	if out.Disabled {
		t.Fatalf("Disabled = true, want false: %s", out.Reason)
	}
	if out.Encoded == 0 {
		t.Fatal("Encoded = 0, want > 0")
	}
	if got := countEmbedRebuildVectors(t, ctx, db); got != out.Encoded {
		t.Fatalf("vector rows = %d, want encoded count %d", got, out.Encoded)
	}
}

func newEmbedRebuildTestDB(t *testing.T, projectID string) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lore.db")
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.MigrateTo(ctx, db, "test", nil); err != nil {
		t.Fatalf("storage.MigrateTo: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path) VALUES (?, ?)`,
		projectID, "/fake/"+projectID,
	); err != nil {
		t.Fatalf("insert project %q: %v", projectID, err)
	}
	return dbPath, db
}

func enableEmbedRebuildMeta(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, kv := range []struct {
		key   string
		value string
	}{
		{"embedder_state", string(embed.EmbedderStateEnabled)},
		{"embedder_model_id", embedRebuildTestModelID},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			kv.key, kv.value,
		); err != nil {
			t.Fatalf("set meta %s: %v", kv.key, err)
		}
	}
}

func seedEmbedRebuildEntries(t *testing.T, ctx context.Context, db *sql.DB, projectID string) []int64 {
	t.Helper()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	entryIDs := make([]int64, 0, 2)
	for i, title := range []string{"first rebuild entry", "second rebuild entry"} {
		res, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID: projectID,
			Kind:      KindObservation,
			Title:     title,
			Summary:   title + " has deterministic content for vector rebuild testing.",
			Topic:     "embed-rebuild",
			Now:       now.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("inscribe %q: %v", title, err)
		}
		entryIDs = append(entryIDs, res.Entry.ID)
	}
	return entryIDs
}

func insertEmbedRebuildVectors(t *testing.T, ctx context.Context, db *sql.DB, entryIDs []int64) {
	t.Helper()
	for _, entryID := range entryIDs {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO lore_vectors
			   (entry_id, model_id, dim, vec, encoded_at, content_hash)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			entryID, embedRebuildTestModelID, 64, make([]byte, 64), time.Now().UTC().Unix(), "hash",
		); err != nil {
			t.Fatalf("insert lore_vectors for entry %d: %v", entryID, err)
		}
	}
}

func countEmbedRebuildVectors(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lore_vectors`).Scan(&count); err != nil {
		t.Fatalf("count lore_vectors: %v", err)
	}
	return count
}
