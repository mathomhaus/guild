package storage

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// TestMigrate_FirstRunAppliesInit verifies the happy path: fresh DB,
// Migrate applies every embedded migration, schema_migrations records
// each, and the 🔧 upgrade notice fires once per migration.
//
// The assertion accepts any monotonically-increasing applied-versions
// list starting at 1. Adding a new NNN_*.up.sql file extends the list
// without churning this test.
func TestMigrate_FirstRunAppliesInit(t *testing.T) {
	ctx := context.Background()
	db := openFreshDB(t)

	var buf bytes.Buffer
	if err := MigrateTo(ctx, db, "lore", &buf); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify schema_migrations has a row per embedded migration file.
	rows, err := queryVersions(ctx, db)
	if err != nil {
		t.Fatalf("queryVersions: %v", err)
	}
	embedded, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(rows) != len(embedded) {
		t.Errorf("applied versions = %v, want %d rows matching embedded corpus", rows, len(embedded))
	}
	for i, m := range embedded {
		if i >= len(rows) || rows[i] != m.version {
			t.Errorf("applied versions[%d] = %d, want %d", i, rows[i], m.version)
		}
	}

	// Verify the 🔧 upgrade line fires for migration 1 — the lead notice.
	got := buf.String()
	want := "🔧 applied lore schema migration 1 (init)...\n"
	if !strings.Contains(got, want) {
		t.Errorf("upgrade output:\n got: %q\nwant contains: %q", got, want)
	}
}

// TestMigrate_IdempotentRerun asserts that calling Migrate twice on the
// same DB emits the 🔧 notices only on the first call. This locks the
// self-heal invariant: on every command after upgrade we re-call Migrate;
// silent no-op is mandatory once the migration has landed.
func TestMigrate_IdempotentRerun(t *testing.T) {
	ctx := context.Background()
	db := openFreshDB(t)

	var buf1, buf2 bytes.Buffer
	if err := MigrateTo(ctx, db, "lore", &buf1); err != nil {
		t.Fatalf("Migrate #1: %v", err)
	}
	if err := MigrateTo(ctx, db, "lore", &buf2); err != nil {
		t.Fatalf("Migrate #2: %v", err)
	}

	if !strings.Contains(buf1.String(), "🔧 applied lore schema migration 1") {
		t.Errorf("first Migrate missed 🔧 line: %q", buf1.String())
	}
	if buf2.Len() != 0 {
		t.Errorf("second Migrate emitted upgrade line: %q", buf2.String())
	}

	// schema_migrations has one row per embedded migration (no double-apply
	// on rerun).
	rows, err := queryVersions(ctx, db)
	if err != nil {
		t.Fatalf("queryVersions: %v", err)
	}
	embedded, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(rows) != len(embedded) {
		t.Errorf("applied versions after rerun = %v, want %d rows", rows, len(embedded))
	}
}

// TestMigrate_SchemaObjects_MaterializedAfterInit asserts that every
// table/trigger/virtual-table named in 001_init.up.sql actually exists
// after Migrate returns. Protects against a silent tx rollback that
// nonetheless records the version in schema_migrations.
func TestMigrate_SchemaObjects_MaterializedAfterInit(t *testing.T) {
	ctx := context.Background()
	db := openFreshDB(t)
	if err := MigrateTo(ctx, db, "", nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	wantObjects := map[string]string{
		"projects":    "table",
		"entries":     "table",
		"entry_links": "table",
		"entries_fts": "table", // FTS5 virtual table shows type='table'
		"task_status": "table",
		"task_notes":  "table",
		"task_events": "table",
		"entries_ai":  "trigger",
		"entries_ad":  "trigger",
		"entries_au":  "trigger",
	}
	for name, kind := range wantObjects {
		var got string
		err := db.QueryRowContext(ctx,
			`SELECT type FROM sqlite_master WHERE name = ?`, name,
		).Scan(&got)
		if err != nil {
			t.Errorf("lookup %s: %v", name, err)
			continue
		}
		if got != kind {
			t.Errorf("%s: type=%s, want %s", name, got, kind)
		}
	}
}

// TestMigrate_SelfHealOnBinaryUpgrade simulates the self-heal contract:
// a new binary ships with an extra migration file. On the first
// command after the upgrade we re-run Migrate; it should detect the
// delta, apply the new migration, and emit a single 🔧 line.
//
// We simulate the "old binary" with an FS that only has 001, then swap
// in an overlay that adds a new 002 file on top. Avoids the real embed
// entirely so the test stays stable regardless of what lives in the
// embed corpus.
func TestMigrate_SelfHealOnBinaryUpgrade(t *testing.T) {
	ctx := context.Background()
	db := openFreshDB(t)

	// --- "Old binary" — only 001_init exists in a fstest overlay. ---
	realInit, err := fs.ReadFile(migrationFS, "migrations/001_init.up.sql")
	if err != nil {
		t.Fatalf("read real 001: %v", err)
	}
	oldOverlay := fstest.MapFS{
		"migrations/001_init.up.sql": &fstest.MapFile{Data: realInit},
	}
	restoreOld := swapMigrationFS(t, oldOverlay)

	var firstRun bytes.Buffer
	if err := MigrateTo(ctx, db, "lore", &firstRun); err != nil {
		restoreOld()
		t.Fatalf("first migrate: %v", err)
	}
	if !strings.Contains(firstRun.String(),
		"🔧 applied lore schema migration 1 (init)...") {
		restoreOld()
		t.Fatalf("expected migration 1 notice, got: %q", firstRun.String())
	}
	restoreOld()

	// --- "User upgrades the binary" — a new migration ships. ---
	//
	// The new overlay keeps the real 001 file content (so mismatched
	// hashes don't trip a future check) plus a brand-new 003 file that
	// ADDs a column to entries. Version 2 is intentionally skipped to
	// assert sparse version spaces work.
	newOverlay := fstest.MapFS{
		"migrations/001_init.up.sql": &fstest.MapFile{Data: realInit},
		"migrations/003_add_example_column.up.sql": &fstest.MapFile{
			Data: []byte(
				"-- simulated upgrade migration\n" +
					"ALTER TABLE entries ADD COLUMN example_upgrade_col TEXT;\n",
			),
		},
	}
	restore := swapMigrationFS(t, newOverlay)
	defer restore()

	// --- First command after upgrade — self-heal fires. ---
	var secondRun bytes.Buffer
	if err := MigrateTo(ctx, db, "lore", &secondRun); err != nil {
		t.Fatalf("self-heal migrate: %v", err)
	}
	wantLine := "🔧 applied lore schema migration 3 (add example column)...\n"
	if secondRun.String() != wantLine {
		t.Errorf("self-heal output:\n got: %q\nwant: %q",
			secondRun.String(), wantLine)
	}

	// Verify the new column exists (confirms the SQL actually ran in tx).
	cols, err := columnNames(ctx, db, "entries")
	if err != nil {
		t.Fatalf("columnNames: %v", err)
	}
	if !contains(cols, "example_upgrade_col") {
		t.Errorf("example_upgrade_col missing from entries: %v", cols)
	}

	// schema_migrations should now hold versions [1, 3] (2 was skipped
	// by design — we're simulating a sparse version space).
	rows, err := queryVersions(ctx, db)
	if err != nil {
		t.Fatalf("queryVersions: %v", err)
	}
	if len(rows) != 2 || rows[0] != 1 || rows[1] != 3 {
		t.Errorf("applied versions = %v, want [1 3]", rows)
	}

	// --- Third command — no pending work, no 🔧 line. ---
	var thirdRun bytes.Buffer
	if err := MigrateTo(ctx, db, "lore", &thirdRun); err != nil {
		t.Fatalf("post-heal migrate: %v", err)
	}
	if thirdRun.Len() != 0 {
		t.Errorf("stable-state Migrate emitted output: %q", thirdRun.String())
	}
}

// TestMigrate_FailedMigrationRollsBack asserts that a syntactically-
// broken migration leaves schema_migrations untouched and the DB in its
// pre-migration state (atomicity).
//
// Implementation: both the baseline run AND the broken-migration run
// share the same overlay (001 only + a broken 002). Running the baseline
// against the overlay rather than the real embed keeps this test stable
// regardless of future migrations in the embed corpus.
func TestMigrate_FailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openFreshDB(t)

	realInit, err := fs.ReadFile(migrationFS, "migrations/001_init.up.sql")
	if err != nil {
		t.Fatalf("read real 001: %v", err)
	}
	baseline := fstest.MapFS{
		"migrations/001_init.up.sql": &fstest.MapFile{Data: realInit},
	}
	restoreBaseline := swapMigrationFS(t, baseline)
	if err := MigrateTo(ctx, db, "", nil); err != nil {
		restoreBaseline()
		t.Fatalf("baseline migrate: %v", err)
	}
	restoreBaseline()

	// Overlay a broken 002 on top of the real 001.
	overlay := fstest.MapFS{
		"migrations/001_init.up.sql": &fstest.MapFile{Data: realInit},
		"migrations/002_broken.up.sql": &fstest.MapFile{
			Data: []byte("THIS IS NOT VALID SQL;\n"),
		},
	}
	restore := swapMigrationFS(t, overlay)
	defer restore()

	err = MigrateTo(ctx, db, "lore", nil)
	if err == nil {
		t.Fatal("expected Migrate to fail on broken 002")
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Errorf("error doesn't reference version 2: %v", err)
	}

	// schema_migrations still holds [1] only.
	rows, err := queryVersions(ctx, db)
	if err != nil {
		t.Fatalf("queryVersions: %v", err)
	}
	if len(rows) != 1 || rows[0] != 1 {
		t.Errorf("schema_migrations after failed 002 = %v, want [1]", rows)
	}
}

// TestLoadMigrations_SortsByVersion verifies the filename-sort property:
// even if fs.ReadDir returns entries out of order, loadMigrations
// returns them by numeric version.
func TestLoadMigrations_SortsByVersion(t *testing.T) {
	overlay := fstest.MapFS{
		"migrations/010_late.up.sql":  &fstest.MapFile{Data: []byte("-- 10")},
		"migrations/001_first.up.sql": &fstest.MapFile{Data: []byte("-- 1")},
		"migrations/003_mid.up.sql":   &fstest.MapFile{Data: []byte("-- 3")},
	}
	restore := swapMigrationFS(t, overlay)
	defer restore()

	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	want := []int{1, 3, 10}
	if len(ms) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(ms), len(want), ms)
	}
	for i, v := range want {
		if ms[i].version != v {
			t.Errorf("ms[%d].version = %d, want %d", i, ms[i].version, v)
		}
	}
}

// TestLoadMigrations_RejectsDuplicateVersion guards against a copy-paste
// slip where two files share a numeric prefix.
func TestLoadMigrations_RejectsDuplicateVersion(t *testing.T) {
	overlay := fstest.MapFS{
		"migrations/001_one.up.sql":     &fstest.MapFile{Data: []byte("-- 1a")},
		"migrations/001_another.up.sql": &fstest.MapFile{Data: []byte("-- 1b")},
	}
	restore := swapMigrationFS(t, overlay)
	defer restore()

	_, err := loadMigrations()
	if err == nil {
		t.Fatal("expected duplicate-version error")
	}
	if !strings.Contains(err.Error(), "duplicate migration version 1") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSplitStatements_TriggerBody verifies the BEGIN…END aware splitter
// keeps CREATE TRIGGER bodies as a single statement even though they
// contain inner semicolons.
func TestSplitStatements_TriggerBody(t *testing.T) {
	script := `
CREATE TABLE t (x INTEGER);
CREATE TRIGGER t_ai AFTER INSERT ON t BEGIN
  INSERT INTO t VALUES (new.x + 1);
  UPDATE t SET x = x * 2 WHERE x > 100;
END;
SELECT 1;
`
	got := splitStatements(script)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3\nstatements = %#v", len(got), got)
	}
	if !strings.Contains(got[1], "BEGIN") || !strings.Contains(got[1], "END") {
		t.Errorf("trigger body lost BEGIN/END: %q", got[1])
	}
	if !strings.Contains(got[1], "x * 2") {
		t.Errorf("trigger body truncated before inner semicolon: %q", got[1])
	}
}

// --- helpers ---

// openFreshDB opens an in-memory DB with the four required pragmas and
// returns it. Unlike openTempDB in db_test.go, this helper does NOT run
// any migration — tests here drive Migrate explicitly.
func openFreshDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migrate.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// swapMigrationFS temporarily replaces the package-level migrationFS with
// any fs.FS for the duration of a test. The restore function MUST be
// deferred by the caller. We use *embed.FS as the declared type in
// migrate.go, so here we wrap the overlay in a shim that satisfies
// fs.ReadDirFS + fs.ReadFileFS.
//
// This is a targeted escape hatch for tests only: see the dedicated
// fsIface declaration in migrate.go (introduced in the test-friendly
// refactor below).
func swapMigrationFS(t *testing.T, overlay fs.FS) func() {
	t.Helper()
	orig := currentMigrationFS
	currentMigrationFS = overlay
	return func() { currentMigrationFS = orig }
}

// queryVersions returns the sorted version list in schema_migrations.
func queryVersions(ctx context.Context, db *sql.DB) ([]int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// columnNames returns every column name in the table via PRAGMA
// table_info. Sort order matches SQLite's cid ordering.
func columnNames(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	// SQL safety: PRAGMA table_info() doesn't accept ? placeholders for
	// its argument, but we can use a parameterized SELECT against
	// sqlite_master as an alternative. Use the well-known pragma_table_info
	// table-valued function that DOES accept a placeholder.
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("pragma_table_info(%s): %w", table, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Compile-time assertion that io.Writer is satisfied by *bytes.Buffer
// (purely so the linter doesn't flag an unused import in a future edit).
var _ io.Writer = (*bytes.Buffer)(nil)
var _ = embed.FS{}

// Historical binaries used version 009 for two unrelated schemas. Keep the
// recorded history and all user payloads while converging both branches.
func TestMigrate_HistoricalVersionNine(t *testing.T) {
	for _, history := range []string{"body", "sleep", "sleep-with-body", "body-with-sleep"} {
		t.Run(history, func(t *testing.T) {
			ctx := context.Background()
			db := openFreshDB(t)
			if _, err := db.Exec(schemaMigrationsDDL); err != nil {
				t.Fatal(err)
			}
			ms, err := loadMigrations()
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				if m.version <= 8 {
					if err := applyOne(ctx, db, m); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := db.Exec(`INSERT INTO projects(id,path) VALUES('fixture','/fixture'); INSERT INTO entries(project_id,topic,kind,title,summary) VALUES('fixture','test','observation','kept','kept summary'); INSERT INTO task_notes(project_id,task_id,agent_id,note) VALUES('fixture','task','fixture','kept note'); INSERT INTO lore_vectors VALUES(1,'fixture',1,X'00000000',1,'kept hash')`); err != nil {
				t.Fatal(err)
			}
			body := strings.HasPrefix(history, "body") || history == "sleep-with-body"
			sleep := strings.HasPrefix(history, "sleep") || history == "body-with-sleep"
			if body {
				if _, err := db.Exec(`ALTER TABLE entries ADD COLUMN body TEXT NOT NULL DEFAULT ''; UPDATE entries SET body='kept body'`); err != nil {
					t.Fatal(err)
				}
			}
			if sleep {
				raw, err := fs.ReadFile(migrationFS, "migrations/009_sleep_journal.up.sql")
				if err != nil {
					t.Fatal(err)
				}
				for _, stmt := range splitStatements(string(raw)) {
					if _, err := db.Exec(stmt); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.Exec(`INSERT INTO sleep_passes VALUES(1,'now',NULL,'autopass',1,NULL)`); err != nil {
					t.Fatal(err)
				}
			}
			description := "lore body"
			if strings.HasPrefix(history, "sleep") {
				description = "sleep journal"
			}
			if _, err := db.Exec(`INSERT INTO schema_migrations(version,description) VALUES(9,?)`, description); err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				if m.version == 10 || m.version == 11 {
					if err := applyOne(ctx, db, m); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := db.Exec(`INSERT INTO task_leases VALUES('fixture','task','session','fixture','now','now','later'); INSERT INTO staleness_signals VALUES(1,'fixture','kept reason','fixture','now')`); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := MigrateTo(ctx, db, "", nil); err != nil {
					t.Fatal(err)
				}
			}
			var got string
			if err := db.QueryRow(`SELECT description FROM schema_migrations WHERE version=9`).Scan(&got); err != nil || got != description {
				t.Fatalf("history changed: %q %v", got, err)
			}
			if err := db.QueryRow(`SELECT body FROM entries WHERE id=1`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if body && got != "kept body" {
				t.Fatalf("body lost: %q", got)
			}
			for table, want := range map[string]int{"entries": 1, "task_notes": 1, "lore_vectors": 1, "task_leases": 1, "staleness_signals": 1, "sleep_passes": 0} {
				if table == "sleep_passes" && sleep {
					want = 1
				}
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
					t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
				}
			}
		})
	}
}

func TestMigrate_ConcurrentStartup(t *testing.T) {
	ctx := context.Background()
	db := openFreshDB(t)
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- MigrateTo(ctx, db, "", nil) }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	versions, err := queryVersions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != len(ms) {
		t.Fatalf("versions=%v", versions)
	}
}
