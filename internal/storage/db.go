package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	// modernc.org/sqlite is a pure-Go SQLite driver (no CGO). Registering
	// the blank import gives us the "sqlite" driver name that sql.Open
	// expects. FTS5 support is compiled in — verified by db_test.go's
	// FTS5 smoke test.
	_ "modernc.org/sqlite"
)

// driverName is the registered name of modernc.org/sqlite's database/sql
// driver. It is NOT "sqlite3" — callers who try to swap in
// github.com/mattn/go-sqlite3 will bring CGO back and break the pure-Go
// binary promise. Keep this package the single source of truth for the
// driver choice.
const driverName = "sqlite"

// requiredPragmas lists the four per-connection pragmas required for every
// guild SQLite connection:
//
//	journal_mode=WAL       writers don't block readers; readers don't block writer
//	busy_timeout=5000      retry transient locks for up to 5s instead of erroring
//	synchronous=NORMAL     WAL-safe durability (FULL is overkill)
//	foreign_keys=ON        enforce referential integrity (entry_links -> entries)
//
// These are injected into the DSN via modernc.org/sqlite's _pragma query
// parameter so every connection in the *sql.DB pool receives them — not
// just the first. Setting them post-open with PRAGMA statements would only
// affect whichever pooled connection happened to serve that Exec call,
// which would only affect whichever pooled connection happened to serve
// that Exec call.
var requiredPragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
	"foreign_keys(ON)",
}

// Open returns a *sql.DB handle to the SQLite database at path, with the
// four required pragmas applied to every pooled connection. path should
// be a filesystem path (e.g. "~/.guild/lore.db" — expand ~ at the call
// site; this package stays IO-pure beyond sql.Open). Pass ":memory:" for
// ephemeral tests.
//
// Open is side-effect free beyond opening the DB: it does NOT run
// migrations. Callers apply migrations with Migrate(ctx, db, description)
// after Open succeeds so the caller stays in control of what gets logged
// ("applying lore migration 002..." vs "applying quest migration 003...").
//
// The returned *sql.DB should be Close()d by the caller.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn, err := buildDSN(path)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}

	// Ping through the context so cancellation/timeouts propagate and so
	// any driver-level error (malformed DSN, unreadable file) surfaces
	// here instead of on first use far from the call site.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping %s: %w", path, err)
	}
	if err := tightenSQLiteFileModes(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: secure %s: %w", path, err)
	}

	return db, nil
}

func tightenSQLiteFileModes(path string) error {
	if path == ":memory:" || strings.HasPrefix(path, ":memory:") {
		return nil
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		p := path + suffix
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// buildDSN turns a filesystem path into a modernc.org/sqlite DSN with the
// four required pragmas encoded as _pragma query parameters. It is
// exported-through-test (via dsnForTest) so db_test.go can assert the
// shape without hitting the disk.
//
// For ":memory:" paths we preserve the special form without a file: URL
// prefix because modernc.org/sqlite's in-memory handling depends on the
// exact string. For all other paths we keep the DSN shape minimal:
// "<path>?_pragma=<p1>&_pragma=<p2>&...".
func buildDSN(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}

	values := url.Values{}
	for _, p := range requiredPragmas {
		values.Add("_pragma", p)
	}

	// :memory: is a magic path — keep it bare-prefixed but still append
	// our pragma query string. modernc.org/sqlite accepts "file::memory:?..."
	// but the simpler ":memory:?..." form also works and matches the
	// tests' expectations.
	base := path
	if strings.Contains(path, "?") {
		return "", fmt.Errorf("path must not contain query string: %s", path)
	}
	return base + "?" + values.Encode(), nil
}

// dsnForTest exposes buildDSN to tests without leaking the helper into
// the public API. Tests in the same package use this to assert the DSN
// shape without calling sql.Open.
func dsnForTest(path string) (string, error) { return buildDSN(path) }
