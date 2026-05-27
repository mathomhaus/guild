package lore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalog_ThreeFiles(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "catalog-proj")

	// Create a temp directory with 3 .md files.
	dir := t.TempDir()
	files := []struct {
		name    string
		content string
	}{
		{
			"first-note.md",
			"# First Note\n\nThis is the first paragraph of the first note with enough words.",
		},
		{
			"second-note.md",
			"# Second Note\n\nThis is the second paragraph of the second note about knowledge.",
		},
		{
			"third-note.md",
			"---\ntitle: Third Note\n---\n\nThis is the third paragraph without frontmatter title.",
		},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.content), 0o600); err != nil {
			t.Fatalf("write %s: %v", f.name, err)
		}
	}

	result, err := Catalog(ctx, db, &CatalogParams{
		Dir:       dir,
		ProjectID: "catalog-proj",
		Topic:     "test-topic",
		Kind:      KindResearch,
	})
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	if result.Imported != 3 {
		t.Errorf("Imported = %d; want 3", result.Imported)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d; want 0", result.Skipped)
	}

	// Verify rows are in the DB.
	var count int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE project_id = 'catalog-proj' AND kind = 'research'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if count != 3 {
		t.Errorf("entries in DB = %d; want 3", count)
	}
}

func TestCatalog_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "catalog-idm")

	dir := t.TempDir()
	fpath := filepath.Join(dir, "note.md")
	if err := os.WriteFile(fpath, []byte("# Note\n\nSome content here."), 0o600); err != nil {
		t.Fatalf("write note.md: %v", err)
	}

	// First run: should import 1.
	r1, err := Catalog(ctx, db, &CatalogParams{Dir: dir, ProjectID: "catalog-idm"})
	if err != nil {
		t.Fatalf("Catalog run 1: %v", err)
	}
	if r1.Imported != 1 {
		t.Errorf("run 1 Imported = %d; want 1", r1.Imported)
	}

	// Second run: same file → should skip 1.
	r2, err := Catalog(ctx, db, &CatalogParams{Dir: dir, ProjectID: "catalog-idm"})
	if err != nil {
		t.Fatalf("Catalog run 2: %v", err)
	}
	if r2.Imported != 0 {
		t.Errorf("run 2 Imported = %d; want 0", r2.Imported)
	}
	if r2.Skipped != 1 {
		t.Errorf("run 2 Skipped = %d; want 1", r2.Skipped)
	}
}

func TestCatalog_KindInference(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "catalog-kind")

	dir := t.TempDir()
	// File with "decision" in the path → KindDecision.
	decisionDir := filepath.Join(dir, "decisions")
	if err := os.MkdirAll(decisionDir, 0o755); err != nil {
		t.Fatalf("mkdir decisions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decisionDir, "adr-001.md"), []byte("# ADR 001\n\nUse SQLite for storage."), 0o600); err != nil {
		t.Fatalf("write adr-001.md: %v", err)
	}
	// Normal file → KindResearch.
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n\nResearch findings here."), 0o600); err != nil {
		t.Fatalf("write notes.md: %v", err)
	}

	result, err := Catalog(ctx, db, &CatalogParams{
		Dir:       dir,
		ProjectID: "catalog-kind",
	})
	if err != nil {
		t.Fatalf("Catalog kind inference: %v", err)
	}
	if result.Imported != 2 {
		t.Fatalf("Imported = %d; want 2", result.Imported)
	}

	var decisionCount, researchCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE project_id = 'catalog-kind' AND kind = 'decision'`,
	).Scan(&decisionCount); err != nil {
		t.Fatalf("count decision: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE project_id = 'catalog-kind' AND kind = 'research'`,
	).Scan(&researchCount); err != nil {
		t.Fatalf("count research: %v", err)
	}
	if decisionCount != 1 {
		t.Errorf("decision count = %d; want 1", decisionCount)
	}
	if researchCount != 1 {
		t.Errorf("research count = %d; want 1", researchCount)
	}
}

func TestCatalog_InvalidKindOverride(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "catalog-bad-kind")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("# Note\n\nSome content here."), 0o600); err != nil {
		t.Fatalf("write note.md: %v", err)
	}

	_, err := Catalog(ctx, db, &CatalogParams{
		Dir:       dir,
		ProjectID: "catalog-bad-kind",
		Kind:      Kind("not-a-real-kind"),
	})
	if !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("Catalog error = %v; want ErrInvalidKind", err)
	}
	if !strings.Contains(err.Error(), "valid: idea, research, decision, observation, principle") {
		t.Fatalf("Catalog error = %q; want valid kind list", err.Error())
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE project_id = 'catalog-bad-kind'`,
	).Scan(&count); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if count != 0 {
		t.Errorf("entries in DB = %d; want 0", count)
	}
}

func TestCatalog_SkipsNonMD(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "catalog-ext")

	dir := t.TempDir()
	// .txt and .go files should be skipped.
	for _, f := range []string{"note.txt", "main.go", "readme.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("content"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	result, err := Catalog(ctx, db, &CatalogParams{Dir: dir, ProjectID: "catalog-ext"})
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	// Only readme.md should be imported.
	if result.Imported != 1 {
		t.Errorf("Imported = %d; want 1 (only .md)", result.Imported)
	}
}

func TestCatalog_NonExistentDir(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "catalog-nodir")

	_, err := Catalog(ctx, db, &CatalogParams{
		Dir:       "/does/not/exist",
		ProjectID: "catalog-nodir",
	})
	if err == nil {
		t.Error("Catalog on non-existent dir should return error")
	}
}
