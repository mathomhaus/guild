package quest

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// beginImmediate acquires a dedicated *sql.Conn from db, then issues
// BEGIN IMMEDIATE with a capped-backoff retry loop.
//
// Callers that open a write transaction with BEGIN DEFERRED take a read
// snapshot the first time they touch the DB. Under concurrent writers this
// means two goroutines can both read stale state and then overwrite each
// other's changes (or surface SQLITE_BUSY when the lock finally contends).
// BEGIN IMMEDIATE serializes writers at lock-acquisition time — no stale
// snapshot, no mid-tx BUSY from a competing writer.
//
// Returns the pinned conn (caller must conn.Close() when done) and a
// rollback function (caller must call if the commit did not succeed).
// Callers use the committed bool pattern:
//
//	conn, rollback, err := beginImmediate(ctx, db)
//	if err != nil { return ..., err }
//	defer conn.Close()
//	committed := false
//	defer rollback(&committed)
//	... writes ...
//	_, err = conn.ExecContext(ctx, "COMMIT")
//	if err != nil { return ..., err }
//	committed = true
func beginImmediate(ctx context.Context, db *sql.DB, opName string) (*sql.Conn, func(*bool), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("quest: %s: acquire conn: %w", opName, err)
	}

	const maxAttempts = 20
	var beginErr error
	for attempt := range maxAttempts {
		_, beginErr = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if beginErr == nil {
			break
		}
		if !storage.IsBusyErr(beginErr) {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("quest: %s: begin immediate: %w", opName, beginErr)
		}
		// Capped linear backoff: 10ms × (attempt+1), max 200ms.
		base := time.Duration(attempt+1) * 10 * time.Millisecond
		if base > 200*time.Millisecond {
			base = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, nil, fmt.Errorf("quest: %s: begin immediate: %w", opName, ctx.Err())
		case <-time.After(base):
		}
	}
	if beginErr != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("quest: %s: begin immediate (contended out after %d attempts): %w",
			opName, maxAttempts, beginErr)
	}

	rollback := func(committed *bool) { //nolint:contextcheck // rollback must survive caller cancellation
		if !*committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	return conn, rollback, nil
}
