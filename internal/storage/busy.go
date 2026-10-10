package storage

import (
	"errors"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsBusy reports whether err is a SQLite busy/locked error from the
// modernc driver that a caller should retry.
//
// It unwraps with errors.As to the driver's typed *sqlite.Error and
// compares the primary result code, so wrapped errors (fmt.Errorf with
// %w through database/sql) still classify. The primary mask folds
// every extended code (BUSY_RECOVERY/SNAPSHOT/
// TIMEOUT, LOCKED_SHAREDCACHE/VTAB) onto its primary (BUSY 5,
// LOCKED 6).
//
// Both BUSY and LOCKED classify as busy, matching the embed helpers'
// stated busy/locked intent: the old substring checks already matched
// shared-cache LOCKED surfaces via "database is locked", and plain
// LOCKED (e.g. DDL against an open reader, "database table is locked
// (6)") is equally transient. Retry budgets at the call sites bound
// the cost, so a persistent lock still surfaces.
//
// String-only errors never classify: passing an error whose message
// merely contains "SQLITE_BUSY" returns false by design.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	// Low 8 bits hold the primary result code per
	// https://www.sqlite.org/rescode.html; high bits carry the extended code.
	const primaryMask = 0xFF
	switch sqliteErr.Code() & primaryMask {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	default:
		return false
	}
}
