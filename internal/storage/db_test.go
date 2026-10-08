package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// openTempDB is a test helper that opens a fresh sqlite DB under t.TempDir
// and runs the canonical 001_init migration. Callers get a ready-to-use
// *sql.DB that auto-closes at test end.
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := MigrateTo(ctx, db, "test", nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// TestOpen_ReturnsUsableHandle verifies Open succeeds for a simple path
// and the returned DB can run a trivial query.
func TestOpen_ReturnsUsableHandle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "smoke.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var one int
	if err := db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if one != 1 {
		t.Fatalf("want 1, got %d", one)
	}
}

// TestBuildDSN_IncludesAllFourPragmas asserts the DSN shape carries
// every required pragma, independent of whether sql.Open picks them up.
// This is the first line of defense against a regression that drops one
// of the four required pragmas from the DSN.
func TestBuildDSN_IncludesAllFourPragmas(t *testing.T) {
	dsn, err := dsnForTest("/tmp/guild-test.db")
	if err != nil {
		t.Fatalf("dsnForTest: %v", err)
	}
	want := []string{
		"_pragma=journal_mode%28WAL%29",
		"_pragma=busy_timeout%285000%29",
		"_pragma=synchronous%28NORMAL%29",
		"_pragma=foreign_keys%28ON%29",
	}
	for _, w := range want {
		if !strings.Contains(dsn, w) {
			t.Errorf("DSN missing pragma %q\ndsn = %s", w, dsn)
		}
	}
}

// TestOpen_AppliesPragmasOnEveryConnection verifies the four required
// pragmas are present on each pooled connection. We open several
// connections concurrently (via db.Conn) and query PRAGMA state inside
// each one; any connection that reports a stale value means the DSN
// pragma injection regressed.
func TestOpen_AppliesPragmasOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pragmas.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Claim several distinct connections simultaneously so the pool
	// actually opens more than one. Go's sql pool reuses connections
	// aggressively, so without this loop the test would only exercise
	// one connection.
	const conns = 4
	var held []*sql.Conn
	for i := 0; i < conns; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn[%d]: %v", i, err)
		}
		held = append(held, c)
	}
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})

	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"}, // SQLite lowercases the journal_mode return
		{"busy_timeout", "5000"},
		{"synchronous", "1"},  // NORMAL == 1 (FULL = 2, OFF = 0)
		{"foreign_keys", "1"}, // ON == 1
	}
	for i, c := range held {
		for _, chk := range checks {
			var got string
			row := c.QueryRowContext(ctx, "PRAGMA "+chk.pragma)
			if err := row.Scan(&got); err != nil {
				t.Fatalf("conn[%d] PRAGMA %s: %v", i, chk.pragma, err)
			}
			if !strings.EqualFold(got, chk.want) {
				t.Errorf("conn[%d] PRAGMA %s = %q, want %q",
					i, chk.pragma, got, chk.want)
			}
		}
	}
}

// TestFTS5_Smoke exercises the CREATE VIRTUAL TABLE … USING fts5 pathway
// end-to-end on a real migrated DB. It proves that:
//
//	(a) modernc.org/sqlite's bundled FTS5 module is registered,
//	(b) our 001_init trigger trio keeps the FTS index synced on insert,
//	    update, and delete.
//
// Each step is asserted independently so a regression points at the exact
// trigger that broke.
func TestFTS5_Smoke(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)

	// Prerequisite: a project row (entries has FK to projects.id).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path) VALUES (?, ?)`,
		"guild-test", "/tmp/guild-test"); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// 1. INSERT + entries_ai trigger must populate entries_fts.
	res, err := db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary)
		 VALUES (?, ?, ?, ?, ?)`,
		"guild-test", "testing", "observation", "alpha", "beta gamma delta")
	if err != nil {
		t.Fatalf("insert entry: %v", err)
	}
	entryID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	assertFTSMatch := func(label, query string, want int64) {
		t.Helper()
		var rowid int64
		err := db.QueryRowContext(ctx,
			`SELECT rowid FROM entries_fts WHERE entries_fts MATCH ?`,
			query,
		).Scan(&rowid)
		if err != nil {
			t.Fatalf("[%s] MATCH %q: %v", label, query, err)
		}
		if rowid != want {
			t.Errorf("[%s] MATCH %q rowid = %d, want %d",
				label, query, rowid, want)
		}
	}

	assertFTSMatch("post-insert", "beta", entryID)
	assertFTSMatch("post-insert multi-token", "gamma delta", entryID)

	// 2. UPDATE of the indexed columns must replace the FTS row.
	if _, err := db.ExecContext(ctx,
		`UPDATE entries SET summary = ? WHERE id = ?`,
		"epsilon zeta", entryID); err != nil {
		t.Fatalf("update entry: %v", err)
	}
	assertFTSMatch("post-update-new", "epsilon", entryID)

	// Confirm the old summary no longer matches (the entries_au trigger
	// first deletes the stale FTS row, then inserts the fresh one).
	var stale int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries_fts WHERE entries_fts MATCH ?`,
		"beta",
	).Scan(&stale)
	if err != nil {
		t.Fatalf("post-update stale MATCH: %v", err)
	}
	if stale != 0 {
		t.Errorf("old FTS row still matches 'beta': %d rows", stale)
	}

	// 3. UPDATE of a non-indexed column (access_count) MUST NOT touch the
	// FTS index. We assert MATCH still returns the fresh row.
	if _, err := db.ExecContext(ctx,
		`UPDATE entries SET access_count = access_count + 1 WHERE id = ?`,
		entryID); err != nil {
		t.Fatalf("bump access_count: %v", err)
	}
	assertFTSMatch("post-bump", "epsilon", entryID)

	// 4. DELETE must cascade to entries_fts via entries_ad.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entries WHERE id = ?`, entryID); err != nil {
		t.Fatalf("delete entry: %v", err)
	}
	var remaining int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries_fts WHERE entries_fts MATCH ?`,
		"epsilon",
	).Scan(&remaining)
	if err != nil {
		t.Fatalf("post-delete MATCH: %v", err)
	}
	if remaining != 0 {
		t.Errorf("FTS row survived DELETE: %d rows", remaining)
	}
}

// TestConcurrentWriters exercises the WAL + busy_timeout contract by
// hammering a shared lore.db with real parallel writers. Each goroutine
// owns an exclusive value slot; we assert every slot lands in the DB
// without errors and without collisions.
//
// Design-notes for the reviewer:
//
//   - ≥4 goroutines (numWriters below) writing in parallel. None of them
//     holds a dedicated connection across the whole loop — each INSERT
//     goes through the shared *sql.DB pool, which is the path production
//     code takes.
//
//   - Each goroutine writes numWrites rows, each with a distinct
//     (writer_id, seq_no) payload. After the run we SELECT distinct
//     (writer_id, seq_no) counts to prove no row was overwritten or lost.
//
//   - A separate connection holds SQLite's write lock while the writers
//     enter db.ExecContext. All six must occupy pooled connections before
//     that lock is released. This proves concurrently pending writes without
//     assuming that SQLite's serialized successful writes overlap in time.
//
//   - Runs with t.Parallel() off because we want predictable scheduling
//     across goroutines here; parallel with other Parallel() tests is fine.
//
// Run with `-race` to catch any data race in our setup code.
func TestConcurrentWriters(t *testing.T) {
	const (
		numWriters = 6
		numWrites  = 40
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := openTempDB(t)

	// Seed a project row so FK checks on any future writer variant pass.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path) VALUES (?, ?)`,
		"concurrent", "/tmp/concurrent"); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Use a dedicated scratch table with no FK dependency so we're
	// measuring pool contention, not trigger cost.
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE scratch (
		    writer_id INTEGER NOT NULL,
		    seq_no    INTEGER NOT NULL,
		    payload   TEXT    NOT NULL,
		    PRIMARY KEY (writer_id, seq_no)
		 )`); err != nil {
		t.Fatalf("create scratch: %v", err)
	}

	// Warm enough connections before taking the write lock. Otherwise a
	// writer could contend during connection initialization, before its
	// busy_timeout pragma has been applied. Writers still use db.ExecContext
	// for every insert; only the controller keeps a dedicated connection.
	db.SetMaxIdleConns(numWriters + 1)
	var held []*sql.Conn
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		if len(held) > 0 {
			//nolint:contextcheck // cleanup must release the write lock even after the scenario context expires
			_, _ = held[0].ExecContext(context.Background(), "ROLLBACK")
		}
		for _, conn := range held {
			_ = conn.Close()
		}
		wg.Wait()
	})
	for i := 0; i <= numWriters; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("warm connection %d: %v", i, err)
		}
		held = append(held, conn)
	}
	blocker := held[0]
	for _, conn := range held[1:] {
		_ = conn.Close()
	}
	if _, err := blocker.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold write lock: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, numWriters*numWrites)
	wg.Add(numWriters)
	wallStart := time.Now()
	for w := 0; w < numWriters; w++ {
		writerID := w
		go func() {
			defer wg.Done()
			<-start // wait for the batch starting pistol
			for s := 0; s < numWrites; s++ {
				payload := fmt.Sprintf("w=%d s=%d ts=%d",
					writerID, s, time.Now().UnixNano())
				_, err := db.ExecContext(ctx,
					`INSERT INTO scratch (writer_id, seq_no, payload)
					 VALUES (?, ?, ?)`,
					writerID, s, payload)
				if err != nil {
					errs <- fmt.Errorf("writer %d seq %d: %w",
						writerID, s, err)
					return
				}
			}
		}()
	}
	close(start)
	// Only the controller and these writers use the pool. With SQLite's
	// write lock held, all six leased writer connections represent active
	// write attempts that cannot yet succeed. A fast writer cannot finish
	// its loop before another writer starts and invalidate this proof.
	for db.Stats().InUse != numWriters+1 {
		if err := ctx.Err(); err != nil {
			t.Fatalf("writers did not all enter the database while locked: stats=%+v: %v", db.Stats(), err)
		}
		runtime.Gosched()
	}
	var beforeRelease int
	if err := blocker.QueryRowContext(ctx, `SELECT COUNT(*) FROM scratch`).Scan(&beforeRelease); err != nil {
		t.Fatalf("count while locked: %v", err)
	}
	if beforeRelease != 0 {
		t.Fatalf("%d writes succeeded before the controller released its lock", beforeRelease)
	}
	if _, err := blocker.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("release write lock: %v", err)
	}
	_ = blocker.Close()
	wg.Wait()
	wallElapsed := time.Since(wallStart)
	close(errs)
	t.Logf("concurrent writers: %d pending writers, %d inserts each = %d rows in %s",
		numWriters, numWrites, numWriters*numWrites, wallElapsed)

	for e := range errs {
		t.Errorf("concurrent write error: %v", e)
	}

	// Verify every (writer_id, seq_no) pair made it in exactly once.
	var total int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scratch`).Scan(&total); err != nil {
		t.Fatalf("count scratch: %v", err)
	}
	if want := numWriters * numWrites; total != want {
		t.Errorf("scratch row count = %d, want %d", total, want)
	}

	// And every writer's sequence is contiguous [0, numWrites).
	for w := 0; w < numWriters; w++ {
		var cnt int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM scratch WHERE writer_id = ?`, w,
		).Scan(&cnt)
		if err != nil {
			t.Fatalf("writer %d count: %v", w, err)
		}
		if cnt != numWrites {
			t.Errorf("writer %d landed %d rows, want %d", w, cnt, numWrites)
		}
	}
}

// TestWALJournalModeOnDisk opens a real file-backed DB and checks that
// sqlite reports journal_mode=wal at the database level (not just the
// connection level). journal_mode=WAL is a persistent mode that sticks
// with the file; if our DSN injection regressed to a connection-only
// path, a second Open on the same file would revert to DELETE.
func TestWALJournalModeOnDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.db")

	db1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Force an actual write so WAL is committed to disk.
	if _, err := db1.ExecContext(ctx, `CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	_ = db1.Close()

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	var mode string
	if err := db2.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode after reopen = %q, want wal", mode)
	}
}
