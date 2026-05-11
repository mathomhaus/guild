package storage

import "errors"

const (
	sqliteBusyCode   = 5
	sqliteLockedCode = 6
)

// IsBusyErr reports whether err is a SQLite busy/locked condition that may be
// retried.
func IsBusyErr(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr interface{ Code() int }
	if !errors.As(err, &sqliteErr) {
		return false
	}

	baseCode := sqliteErr.Code() & 0xFF
	return baseCode == sqliteBusyCode || baseCode == sqliteLockedCode
}
