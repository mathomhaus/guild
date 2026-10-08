package storage

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// TestMigrateFS_Isolation proves the ADR-006 storage-isolation property:
// two modules, each with its own migration corpus applied to its own
// database, do not contaminate each other. dbA ends up with only fsA's
// table and dbB with only fsB's — the shared-corpus foot-gun (every table
// landing in every DB) is gone once a caller passes an explicit fs.FS.
func TestMigrateFS_Isolation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	fsA := fstest.MapFS{
		"migrations/001_create_alpha.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE alpha (id INTEGER PRIMARY KEY);"),
		},
	}
	fsB := fstest.MapFS{
		"migrations/001_create_beta.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE beta (id INTEGER PRIMARY KEY);"),
		},
	}

	dbA, err := Open(ctx, filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatalf("open a.db: %v", err)
	}
	defer func() { _ = dbA.Close() }()
	dbB, err := Open(ctx, filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatalf("open b.db: %v", err)
	}
	defer func() { _ = dbB.Close() }()

	if err := MigrateFS(ctx, dbA, fsA, "alpha", io.Discard); err != nil {
		t.Fatalf("MigrateFS a: %v", err)
	}
	if err := MigrateFS(ctx, dbB, fsB, "beta", io.Discard); err != nil {
		t.Fatalf("MigrateFS b: %v", err)
	}

	if !hasTable(ctx, t, dbA, "alpha") {
		t.Errorf("dbA missing its own table alpha")
	}
	if hasTable(ctx, t, dbA, "beta") {
		t.Errorf("dbA leaked table beta from the other module's corpus")
	}
	if !hasTable(ctx, t, dbB, "beta") {
		t.Errorf("dbB missing its own table beta")
	}
	if hasTable(ctx, t, dbB, "alpha") {
		t.Errorf("dbB leaked table alpha from the other module's corpus")
	}
}

// hasTable reports whether a user table named tbl exists in db.
func hasTable(ctx context.Context, t *testing.T, db *sql.DB, tbl string) bool {
	t.Helper()
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		tbl,
	).Scan(&n)
	if err != nil {
		t.Fatalf("hasTable %q: %v", tbl, err)
	}
	return n > 0
}
