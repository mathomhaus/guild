package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// migration describes one numbered SQL file under migrations/. Parsed from
// its filename: NNN_description.up.sql -> version=NNN, description="description".
type migration struct {
	version     int
	description string
	filename    string
}

// fileNameRe matches "NNN_description.up.sql" and captures both halves.
// NNN must be ≥ 1 digit (we pad to 3 in practice but don't enforce here).
// description is any run of [a-z0-9_] characters — Migrate lowercases
// whatever it parses so callers don't accidentally emit mixed-case lines.
var fileNameRe = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.up\.sql$`)

// schemaMigrationsDDL creates the tracking table on demand. version is
// an INTEGER PK; description and applied_at are audit-only.
const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version     INTEGER PRIMARY KEY,
  description TEXT    NOT NULL,
  applied_at  TEXT    NOT NULL DEFAULT (datetime('now'))
)`

// Migrate applies every pending migration from the embedded migrations/
// directory to db, in ascending version order, inside a transaction per
// migration. Migrations already recorded in schema_migrations are skipped.
//
// The logLine argument is used as a prefix for the one-shot "🔧 applied
// schema migration ..." notice. Pass something like "lore" or "quest" so
// users who run with both DBs see which DB each upgrade line refers to.
// Pass an empty string to suppress the DB-scope prefix.
//
// Migrate is safe to call on every startup: if no migrations are pending
// it's a no-op beyond schema_migrations creation + one COUNT(*) lookup
// per migration. That makes it suitable for a self-heal-on-first-command
// flow after a binary upgrade.
//
// Writes to logLine go to the io.Writer in the returned logger closure
// (see migrateImpl). Pass nil or io.Discard to mute. For normal CLI use,
// pass os.Stderr so upgrade notices don't pollute --json output on stdout.
func Migrate(ctx context.Context, db *sql.DB, scope string) error {
	return migrateImpl(ctx, db, scope, stderrSink)
}

// MigrateTo is the test-facing variant of Migrate that lets tests capture
// the "🔧 applied schema migration ..." lines without racing on real
// stderr. Production code calls Migrate; tests call MigrateTo with an
// explicit io.Writer.
func MigrateTo(ctx context.Context, db *sql.DB, scope string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	return migrateImpl(ctx, db, scope, out)
}

// MigrateFS is the module-facing variant (ADR-006): it applies the migration
// corpus embedded in fsys to db, rather than the binary's built-in shared
// corpus. fsys must contain a top-level "migrations/" directory of
// NNN_description.up.sql files — the same convention every module's
// //go:embed migrations/*.up.sql produces. Because each module passes its
// own fsys (and opens its own DB), a module's tables land only in that
// module's database instead of leaking into a shared corpus. scope is the
// log-line prefix (pass the module/db name); out receives the 🔧 upgrade
// notices (nil is treated as io.Discard).
func MigrateFS(ctx context.Context, db *sql.DB, fsys fs.FS, scope string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	return migrateImplFS(ctx, db, fsys, scope, out)
}

func migrateImpl(ctx context.Context, db *sql.DB, scope string, out io.Writer) error {
	return migrateImplFS(ctx, db, currentMigrationFS, scope, out)
}

// migrateImplFS is the core runner: it applies every pending migration from
// fsys (which must hold a top-level "migrations/" directory) to db. The fsys
// parameter is what lets each module own its corpus (ADR-006); the legacy
// Migrate/MigrateTo path passes currentMigrationFS for byte-identical
// behavior.
func migrateImplFS(ctx context.Context, db *sql.DB, fsys fs.FS, scope string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	if _, err := db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("storage: migrate: create schema_migrations: %w", err)
	}

	migrations, err := loadMigrationsFS(fsys)
	if err != nil {
		return fmt.Errorf("storage: migrate: load migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("storage: migrate: read applied versions: %w", err)
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := applyOneFS(ctx, db, fsys, m); err != nil {
			return err
		}
		// Emit the one-time upgrade line. This fires exactly once per
		// migration per database because applied[] is re-read from disk
		// on the next call.
		prefix := ""
		if scope != "" {
			prefix = scope + " "
		}
		fmt.Fprintf(out, "🔧 applied %sschema migration %d (%s)...\n",
			prefix, m.version, m.description)
	}

	return nil
}

// stderrSink is a package-level sentinel that migrateImpl uses as the
// default output destination. We resolve it lazily so tests that swap
// out os.Stderr via -rerun tricks can still intercept it. In practice
// this is just os.Stderr — see default_sink.go.
var stderrSink = defaultSink()

// appliedVersions returns the set of migration versions recorded in
// schema_migrations.
func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return out, nil
}

// currentMigrationFS is the fs.FS Migrate reads from. In production it
// points at the embedded migrationFS. Tests swap this in-place via
// swapMigrationFS to simulate "a new binary shipped with more migrations"
// without mutating the embed.FS (which Go won't let us do anyway). Keep
// the swap test-only — production code never mutates this variable.
var currentMigrationFS fs.FS = migrationFS

// loadMigrations walks currentMigrationFS and returns the migrations
// sorted by version. It is deterministic: same embedded corpus -> same
// slice.
func loadMigrations() ([]migration, error) {
	return loadMigrationsFS(currentMigrationFS)
}

// loadMigrationsFS walks fsys's "migrations/" directory and returns the
// migrations sorted by version. Deterministic: same corpus -> same slice.
func loadMigrationsFS(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations dir: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		match := fileNameRe.FindStringSubmatch(name)
		if match == nil {
			// Ignore unrelated files (e.g. *.down.sql if ever added,
			// README.md, editor swap files). Enforcement of the
			// naming convention happens at commit time, not runtime.
			continue
		}
		v, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("parse version in %q: %w", name, err)
		}
		if prior, dup := seen[v]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q",
				v, prior, name)
		}
		seen[v] = name
		out = append(out, migration{
			version:     v,
			description: strings.ReplaceAll(match[2], "_", " "),
			filename:    name,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// applyOne executes every statement in migration m inside a single
// transaction and records the version in schema_migrations. Either all
// statements land or none do — SQLite rolls the whole tx back on error.
func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	return applyOneFS(ctx, db, currentMigrationFS, m)
}

// applyOneFS executes migration m, read from fsys, inside a single
// transaction and records its version. applyOne is the legacy
// currentMigrationFS-bound wrapper.
func applyOneFS(ctx context.Context, db *sql.DB, fsys fs.FS, m migration) error {
	raw, err := fs.ReadFile(fsys, "migrations/"+m.filename)
	if err != nil {
		return fmt.Errorf("storage: migrate: read %s: %w", m.filename, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: migrate: begin tx (version %d): %w", m.version, err)
	}
	// Rollback is a no-op if Commit already ran; safe to always-defer.
	defer func() { _ = tx.Rollback() }()

	// Acquire SQLite's writer lock before reading the version. A deferred
	// transaction that reads first cannot safely upgrade its snapshot when
	// another process migrates concurrently.
	if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET version = version WHERE 0`); err != nil {
		return fmt.Errorf("storage: migrate: lock version %d: %w", m.version, err)
	}
	var recorded int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&recorded); err != nil {
		return err
	}
	if recorded != 0 {
		return tx.Commit()
	}

	// Version 009 historically meant either body or sleep journal. Version
	// 012 converges the body schema without rewriting that audit history or
	// treating a duplicate-column error as a general migration success.
	if m.filename == "012_lore_body.up.sql" {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('entries') WHERE name = 'body'`).Scan(&present); err != nil {
			return err
		}
		if present != 0 {
			raw = nil
		}
	}

	for i, stmt := range splitStatements(string(raw)) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("storage: migrate: version %d statement %d: %w",
				m.version, i+1, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, description, applied_at) VALUES (?, ?, ?)`,
		m.version, m.description, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("storage: migrate: record version %d: %w", m.version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: migrate: commit version %d: %w", m.version, err)
	}
	return nil
}

// splitStatements splits a SQL script into individual statements. It
// handles CREATE TRIGGER bodies (which embed semicolons inside BEGIN…END)
// by tracking depth: a statement only ends at ";" when the BEGIN…END
// nesting depth is zero. SQL line comments ("-- ...") are stripped so
// they don't confuse the depth tracker.
//
// Assumptions (hold for every migration in migrations/ today; keep them
// holding when adding new files):
//
//  1. No string literal contains the unquoted words BEGIN or END.
//  2. No identifier starts with BEGIN or END as a prefix at column 0.
//  3. No inline "-- ..." comment appears inside a string literal.
//
// Prefer block comments ("/* ... */") for inline commentary on lines
// that also contain data or trigger bodies.
func splitStatements(script string) []string {
	var cleaned strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			cleaned.WriteByte('\n')
			continue
		}
		if idx := strings.Index(line, " --"); idx >= 0 {
			line = line[:idx]
		}
		cleaned.WriteString(line)
		cleaned.WriteByte('\n')
	}
	src := cleaned.String()
	upper := strings.ToUpper(src)

	var (
		stmts []string
		buf   strings.Builder
		depth int
	)

	isBoundary := func(b byte) bool {
		return b == 0 || b == ' ' || b == '\n' || b == '\r' || b == '\t' || b == ';'
	}

	for i := 0; i < len(src); i++ {
		if i+5 <= len(upper) && upper[i:i+5] == "BEGIN" {
			var next byte
			if i+5 < len(upper) {
				next = upper[i+5]
			}
			// Guard the left boundary too so "BEGIN" at col 0 or
			// after whitespace is fine but "foobegin" is ignored.
			var prev byte
			if i > 0 {
				prev = upper[i-1]
			}
			if isBoundary(next) && (i == 0 || isBoundary(prev)) {
				depth++
			}
		}
		if i+3 <= len(upper) && upper[i:i+3] == "END" {
			var next byte
			if i+3 < len(upper) {
				next = upper[i+3]
			}
			var prev byte
			if i > 0 {
				prev = upper[i-1]
			}
			if isBoundary(next) && (i == 0 || isBoundary(prev)) {
				if depth > 0 {
					depth--
				}
			}
		}

		if src[i] == ';' && depth == 0 {
			stmt := strings.TrimSpace(buf.String())
			if stmt != "" {
				stmts = append(stmts, stmt)
			}
			buf.Reset()
			continue
		}
		buf.WriteByte(src[i])
	}
	if stmt := strings.TrimSpace(buf.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}
