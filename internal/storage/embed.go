// Package storage owns the guild SQLite substrate: connection setup with
// the four concurrency pragmas, the embedded migration corpus, and the
// forward-only migration runner. Downstream packages (lore/, quest/, mcp/)
// only ever talk to *sql.DB handles Open returns; they never read this
// package's migration files directly.
package storage

import (
	"embed"
	"io/fs"
)

// migrationFS holds every *.up.sql file under migrations/, numbered
// sequentially (001_init.up.sql, 002_*.up.sql, ...). Migrate walks this
// filesystem in name order and applies any file whose numeric prefix is
// not already recorded in schema_migrations.
//
// Keep the naming strict: NNN_description.up.sql with NNN monotonically
// increasing. The SQL inside each file is applied inside a single
// transaction — see migrate.go.
//
//go:embed migrations/*.up.sql
var migrationFS embed.FS

// DefaultMigrationFS returns the binary's built-in shared migration corpus.
// The legacy Migrate/MigrateTo path uses it implicitly; during the ADR-006
// module transition a module that still wants the shared corpus can pass it
// explicitly to MigrateFS.
func DefaultMigrationFS() fs.FS { return migrationFS }
