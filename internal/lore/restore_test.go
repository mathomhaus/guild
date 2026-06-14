package lore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeTestSnapshot writes a snapshot.json to path with given lore entries and links.
func writeTestSnapshot(t testing.TB, path string, schemaVersion int, loreEntries []snapshotLoreEntry, links []snapshotLoreLink) {
	t.Helper()
	snap := snapshot{
		SchemaVersion: schemaVersion,
		SnapshotAt:    "2026-01-01T00:00:00Z",
		ProducedBy:    "test",
		Lore:          loreEntries,
	}
	// We use the internal snapshot struct but need to set Links separately because
	// snapshot doesn't have a Links field (links are part of lore section in v1).
	// Build the raw JSON with a links key for restore to consume.
	type snapWithLinks struct {
		SchemaVersion int                 `json:"schema_version"`
		SnapshotAt    string              `json:"snapshot_at"`
		ProducedBy    string              `json:"produced_by"`
		Lore          []snapshotLoreEntry `json:"lore"`
		Links         []snapshotLoreLink  `json:"links"`
	}
	doc := snapWithLinks{
		SchemaVersion: snap.SchemaVersion,
		SnapshotAt:    snap.SnapshotAt,
		ProducedBy:    snap.ProducedBy,
		Lore:          snap.Lore,
		Links:         links,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal test snapshot: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write test snapshot: %v", err)
	}
}

func TestRestore_BasicImport(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "restore-proj")

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	writeTestSnapshot(t, snapshotPath, 1, []snapshotLoreEntry{
		{
			ID:        101,
			ProjectID: "old-proj",
			Topic:     "test",
			Kind:      "research",
			Title:     "Research finding one",
			Summary:   "Found something interesting.",
			Status:    "current",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		{
			ID:        102,
			ProjectID: "old-proj",
			Topic:     "arch",
			Kind:      "decision",
			Title:     "Use SQLite for storage",
			Summary:   "SQLite is embedded and requires no server.",
			Status:    "current",
			CreatedAt: "2026-01-02T00:00:00Z",
			UpdatedAt: "2026-01-02T00:00:00Z",
		},
	}, nil)

	result, err := Restore(ctx, db, "restore-proj", snapshotPath)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if result.Imported != 2 {
		t.Errorf("Imported = %d; want 2", result.Imported)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d; want 0", result.Skipped)
	}
	if result.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d; want 1", result.SchemaVersion)
	}

	// Verify entries are in the DB under the new project.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE project_id = 'restore-proj'`,
	).Scan(&count); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if count != 2 {
		t.Errorf("DB entry count = %d; want 2", count)
	}
}

func TestRestore_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "restore-idm")

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	entries := []snapshotLoreEntry{
		{
			ID:        1,
			ProjectID: "old",
			Topic:     "test",
			Kind:      "research",
			Title:     "Unique research title for idempotent test",
			Summary:   "Summary.",
			Status:    "current",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	}
	writeTestSnapshot(t, snapshotPath, 1, entries, nil)

	r1, err := Restore(ctx, db, "restore-idm", snapshotPath)
	if err != nil {
		t.Fatalf("Restore run 1: %v", err)
	}
	if r1.Imported != 1 {
		t.Errorf("run 1 Imported = %d; want 1", r1.Imported)
	}

	r2, err := Restore(ctx, db, "restore-idm", snapshotPath)
	if err != nil {
		t.Fatalf("Restore run 2: %v", err)
	}
	if r2.Imported != 0 {
		t.Errorf("run 2 Imported = %d; want 0 (idempotent)", r2.Imported)
	}
	if r2.Skipped != 1 {
		t.Errorf("run 2 Skipped = %d; want 1", r2.Skipped)
	}
}

func TestRestore_LinkReconstruction(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "restore-links")

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	writeTestSnapshot(t, snapshotPath, 1, []snapshotLoreEntry{
		{
			ID:        10,
			ProjectID: "old",
			Topic:     "test",
			Kind:      "research",
			Title:     "Source research entry",
			Summary:   "This entry informs another.",
			Status:    "current",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		{
			ID:        20,
			ProjectID: "old",
			Topic:     "test",
			Kind:      "decision",
			Title:     "Decision entry that uses the research",
			Summary:   "Based on entry 10.",
			Status:    "current",
			CreatedAt: "2026-01-02T00:00:00Z",
			UpdatedAt: "2026-01-02T00:00:00Z",
		},
	}, []snapshotLoreLink{
		{FromID: 10, ToID: 20, Relation: "informs"},
	})

	result, err := Restore(ctx, db, "restore-links", snapshotPath)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if result.Imported != 2 {
		t.Errorf("Imported = %d; want 2", result.Imported)
	}
	if result.LinksAdded != 1 {
		t.Errorf("LinksAdded = %d; want 1", result.LinksAdded)
	}

	// Verify link is in the DB.
	var linkCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entry_links`,
	).Scan(&linkCount); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if linkCount != 1 {
		t.Errorf("entry_links count = %d; want 1", linkCount)
	}
}

func TestRestore_UnknownSchemaVersion(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "restore-unk")

	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "snapshot.json")

	// Write a snapshot with a future schema version.
	doc := map[string]interface{}{
		"schema_version": 99,
		"snapshot_at":    "2026-01-01T00:00:00Z",
		"produced_by":    "future guild",
		"lore":           []interface{}{},
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(snapshotPath, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	_, err := Restore(ctx, db, "restore-unk", snapshotPath)
	if err == nil {
		t.Error("Restore should return error for unsupported schema_version")
	}
}

func TestRestore_MissingFile(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "restore-missing")

	_, err := Restore(ctx, db, "restore-missing", "/does/not/exist/snapshot.json")
	if err == nil {
		t.Error("Restore on missing file should return error")
	}
}

// TestRestore_BackwardCompatNoLinks confirms that a snapshot.json produced by
// an older binary (no "links" key) restores cleanly with LinksAdded=0, not an
// error. JSON unmarshal leaves Links nil when the key is absent, which the
// restore loop handles by ranging over a nil slice — zero iterations, no panic.
func TestRestore_BackwardCompatNoLinks(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "restore-nolinks")

	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "snapshot.json")

	// Hand-craft a v1 snapshot without the "links" key, mimicking older binaries.
	doc := map[string]interface{}{
		"schema_version": 1,
		"snapshot_at":    "2026-01-01T00:00:00Z",
		"produced_by":    "guild 0.0.9",
		"lore": []map[string]interface{}{
			{
				"id":         1,
				"project_id": "old-proj",
				"topic":      "test",
				"kind":       "research",
				"title":      "Legacy entry without links",
				"summary":    "This snapshot predates link serialisation.",
				"status":     "current",
				"created_at": "2026-01-01T00:00:00Z",
				"updated_at": "2026-01-01T00:00:00Z",
			},
		},
		// deliberately no "links" key
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(snapshotPath, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	result, err := Restore(ctx, db, "restore-nolinks", snapshotPath)
	if err != nil {
		t.Fatalf("Restore on link-less snapshot: %v", err)
	}
	if result.Imported != 1 {
		t.Errorf("Imported = %d; want 1", result.Imported)
	}
	if result.LinksAdded != 0 {
		t.Errorf("LinksAdded = %d; want 0 (no links in snapshot)", result.LinksAdded)
	}
}

func TestArchiveRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "roundtrip-src", "roundtrip-dst")

	// Insert entries in source project.
	_, err := db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, tags, status, created_at, updated_at)
		 VALUES ('roundtrip-src', 'golang', 'principle', 'Go error handling', 'Always check errors explicitly.', 'go,errors', 'current', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("insert src entry: %v", err)
	}

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	if err := Archive(ctx, db, "roundtrip-src", snapshotPath); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	result, err := Restore(ctx, db, "roundtrip-dst", snapshotPath)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if result.Imported != 1 {
		t.Errorf("Imported = %d; want 1", result.Imported)
	}

	// Check the restored entry has the right title.
	var title string
	if err := db.QueryRowContext(ctx,
		`SELECT title FROM entries WHERE project_id = 'roundtrip-dst'`,
	).Scan(&title); err != nil {
		t.Fatalf("read restored title: %v", err)
	}
	if title != "Go error handling" {
		t.Errorf("restored title = %q; want 'Go error handling'", title)
	}
}

// TestArchiveRestoreRoundTrip_PreservesBody guards the backup path: the
// verbatim body must survive archive → restore. Without body in the
// snapshot projection or the restore INSERT, a round-trip would silently
// drop the full source material and leave only the summary.
func TestArchiveRestoreRoundTrip_PreservesBody(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "rtbody-src", "rtbody-dst")

	const fullBody = "line one of the verbatim body\nline two with detail\nline three"

	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "rtbody-src",
		Kind:      KindObservation,
		Title:     "entry carrying a verbatim body",
		Summary:   "short distillation for the search snippet.",
		Body:      fullBody,
		Topic:     "test",
	})
	if err != nil {
		t.Fatalf("inscribe src entry: %v", err)
	}
	_ = res

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	if err := Archive(ctx, db, "rtbody-src", snapshotPath); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := Restore(ctx, db, "rtbody-dst", snapshotPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx,
		`SELECT body FROM entries WHERE project_id = 'rtbody-dst'`,
	).Scan(&body); err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if body != fullBody {
		t.Errorf("restored body = %q; want %q", body, fullBody)
	}
}
