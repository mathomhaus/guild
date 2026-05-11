package lore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// beginImmediate is the lore-package-local helper matching the one in
// internal/quest/tx.go and internal/lore/embed/tx.go. Duplicated (not
// imported) because lore sits below quest and above embed in the
// dependency graph (hexagonal boundary) and pulling either neighbour
// in would flip an arrow. See internal/quest/tx.go for the full
// design rationale.
//
// Caller pattern:
//
//	conn, rollback, err := beginImmediate(ctx, db, "lore: seal")
//	if err != nil { return err }
//	defer conn.Close()
//	committed := false
//	defer rollback(&committed)
//	... writes ...
//	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil { return err }
//	committed = true
func beginImmediate(ctx context.Context, db *sql.DB, opName string) (*sql.Conn, func(*bool), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: acquire conn: %w", opName, err)
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
			return nil, nil, fmt.Errorf("%s: begin immediate: %w", opName, beginErr)
		}
		base := time.Duration(attempt+1) * 10 * time.Millisecond
		if base > 200*time.Millisecond {
			base = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, nil, fmt.Errorf("%s: begin immediate: %w", opName, ctx.Err())
		case <-time.After(base):
		}
	}
	if beginErr != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("%s: begin immediate (contended out after %d attempts): %w",
			opName, maxAttempts, beginErr)
	}

	rollback := func(committed *bool) { //nolint:contextcheck // rollback must survive caller cancellation
		if !*committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	return conn, rollback, nil
}
