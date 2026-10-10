package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestIsBusy_RejectsNilAndPlainErrors guards the typed-sentinel contract:
// nil and string-only errors never classify, even when the message
// mentions SQLITE_BUSY. This is the regression the substring helpers
// could not express.
func TestIsBusy_RejectsNilAndPlainErrors(t *testing.T) {
	if IsBusy(nil) {
		t.Errorf("IsBusy(nil) = true, want false")
	}
	for _, msg := range []string{
		"SQLITE_BUSY",
		"database is locked (5) (SQLITE_BUSY)",
		"database is locked",
		"SQLITE_LOCKED",
		"simulated SQLITE_BUSY: writer-lock contention",
	} {
		if IsBusy(errors.New(msg)) {
			t.Errorf("IsBusy(%q) = true, want false (string-only must not classify)", msg)
		}
		if IsBusy(fmt.Errorf("wrap: %w", errors.New(msg))) {
			t.Errorf("IsBusy(wrapped %q) = true, want false", msg)
		}
	}
}

// TestIsBusy_RejectsNonBusySQLiteError ensures a real typed SQLite error
// with a different code (missing table → SQLITE_ERROR) does not classify.
func TestIsBusy_RejectsNonBusySQLiteError(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "notbusy.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx, `SELECT * FROM missing_table_xyz`)
	if err == nil {
		t.Fatal("expected missing-table error, got nil")
	}
	if IsBusy(err) {
		t.Errorf("IsBusy(missing-table err %v) = true, want false", err)
	}
	if IsBusy(fmt.Errorf("quest: accept: update: %w", err)) {
		t.Errorf("IsBusy(wrapped missing-table err %v) = true, want false", err)
	}
}

// TestIsBusy_AcceptsRealBusyError drives a genuine BEGIN IMMEDIATE
// contention with busy_timeout(0) so the loser fails immediately with a
// typed SQLITE_BUSY. Both the raw and %w-wrapped forms must classify.
func TestIsBusy_AcceptsRealBusyError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")

	// Create the file first through the canonical Open path.
	setup, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open setup: %v", err)
	}
	if _, err := setup.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		_ = setup.Close()
		t.Fatalf("create table: %v", err)
	}
	_ = setup.Close()

	dsn := path + "?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)"
	db1, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	t.Cleanup(func() { _ = db1.Close() })
	db2, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	conn1, err := db1.Conn(ctx)
	if err != nil {
		t.Fatalf("conn1: %v", err)
	}
	defer func() { _ = conn1.Close() }()
	conn2, err := db2.Conn(ctx)
	if err != nil {
		t.Fatalf("conn2: %v", err)
	}
	defer func() { _ = conn2.Close() }()

	if _, err := conn1.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("holder BEGIN IMMEDIATE: %v", err)
	}
	//nolint:contextcheck // cleanup ROLLBACK must survive test context expiry
	t.Cleanup(func() { _, _ = conn1.ExecContext(context.Background(), "ROLLBACK") })

	_, beginErr := conn2.ExecContext(ctx, "BEGIN IMMEDIATE")
	for i := 0; i < 50 && beginErr == nil; i++ {
		_, _ = conn2.ExecContext(ctx, "ROLLBACK")
		time.Sleep(5 * time.Millisecond)
		_, beginErr = conn2.ExecContext(ctx, "BEGIN IMMEDIATE")
	}
	if beginErr == nil {
		_, _ = conn2.ExecContext(ctx, "ROLLBACK")
		t.Fatal("second BEGIN IMMEDIATE never contended; cannot prove BUSY classification")
	}
	if !IsBusy(beginErr) {
		t.Errorf("IsBusy(BEGIN IMMEDIATE contention %v) = false, want true", beginErr)
	}
	if !IsBusy(fmt.Errorf("embed: op: begin immediate: %w", beginErr)) {
		t.Errorf("IsBusy(wrapped contention %v) = false, want true", beginErr)
	}
}

// TestIsBusy_AcceptsRealLockedError drives a genuine SQLITE_LOCKED:
// DDL on a connection with an open reader. Both the raw and %w-wrapped
// forms must classify, and the surfaced code must be LOCKED so the
// test pins the BUSY+LOCKED contract instead of passing on either.
func TestIsBusy_AcceptsRealLockedError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "locked.db")

	setup, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open setup: %v", err)
	}
	if _, err := setup.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		_ = setup.Close()
		t.Fatalf("create table: %v", err)
	}
	if _, err := setup.ExecContext(ctx, `INSERT INTO t (v) VALUES ('a')`); err != nil {
		_ = setup.Close()
		t.Fatalf("insert: %v", err)
	}
	_ = setup.Close()

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.QueryContext(ctx, `SELECT id, v FROM t`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("expected one row")
	}

	_, lockErr := conn.ExecContext(ctx, `DROP TABLE t`)
	if lockErr == nil {
		t.Fatal("DROP TABLE with open reader succeeded; cannot prove LOCKED classification")
	}
	var sqliteErr *sqlite.Error
	if !errors.As(lockErr, &sqliteErr) {
		t.Fatalf("expected typed *sqlite.Error, got %T: %v", lockErr, lockErr)
	}
	if primary := sqliteErr.Code() & 0xFF; primary != sqlite3.SQLITE_LOCKED {
		t.Fatalf("expected SQLITE_LOCKED (%d), got code %d: %v",
			sqlite3.SQLITE_LOCKED, sqliteErr.Code(), lockErr)
	}
	if !IsBusy(lockErr) {
		t.Errorf("IsBusy(DDL lock %v) = false, want true", lockErr)
	}
	if !IsBusy(fmt.Errorf("quest: lease: %w", lockErr)) {
		t.Errorf("IsBusy(wrapped DDL lock %v) = false, want true", lockErr)
	}
}
