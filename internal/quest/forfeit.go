package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Forfeit releases an in_progress claim on taskID without marking it
// done. Any notes already attached persist for the next agent who
// accepts. Note can be empty; when non-empty it is stored as
// "[released] <note>" so the reason survives into subsequent scrolls.
//
// Status gating (QUEST-135):
//   - status=in_progress → claim is released, event + optional note
//     are emitted, status flips to 'next'. Current-day behavior.
//   - status=next (or any non-in_progress, non-done status) → no-op.
//     No DB writes. Returns ForfeitResult{AlreadyNext: true} so the
//     CLI/MCP adapter can render a neutral "nothing to forfeit" line
//     rather than the misleading ↩️ success glyph.
//   - status=done → returns ErrAlreadyDone. Forfeit refuses to
//     silently reopen a completed quest; the caller should use
//     quest post (rework) to re-do the work explicitly.
func Forfeit(ctx context.Context, db *sql.DB, projectID, taskID, note string) (*ForfeitResult, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: forfeit: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: forfeit: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return nil, fmt.Errorf("quest: forfeit: empty task_id")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// BEGIN IMMEDIATE serializes concurrent Forfeit calls: reads current status
	// and claimed_by, then writes the release — a stale DEFERRED snapshot could
	// cause two goroutines to both read in_progress and both execute the release.
	conn, rollback, err := beginImmediate(ctx, db, "forfeit")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	var existStatus, existOwner sql.NullString
	err = conn.QueryRowContext(ctx,
		`SELECT status, claimed_by FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&existStatus, &existOwner)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, taskID)
		}
		return nil, fmt.Errorf("quest: forfeit: probe existing: %w", err)
	}

	switch Status(existStatus.String) {
	case StatusDone:
		return nil, fmt.Errorf("%w: %s — use quest post to rework", ErrAlreadyDone, taskID)
	case StatusInProgress:
		// fall through to the release path below.
	default:
		// next / blocked / anything else → no-op. Load the quest for
		// the return value but do not touch task_status / task_events.
		q, err := loadTx(ctx, conn, projectID, taskID)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, fmt.Errorf("quest: forfeit: commit: %w", err)
		}
		committed = true
		return &ForfeitResult{Quest: q, AlreadyNext: true}, nil
	}

	owner := existOwner.String
	if owner == "" {
		owner = "agent"
	}

	// Optional note attached first so the `[released]` note precedes
	// any downstream pulse queries that scan forward-in-time.
	if n := strings.TrimSpace(note); n != "" {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			projectID, taskID, owner, "[released] "+n, now,
		); err != nil {
			return nil, fmt.Errorf("quest: forfeit: insert release note: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx,
		`UPDATE task_status
		 SET status = 'next',
		     claimed_by = NULL,
		     claimed_at = NULL,
		     updated_at = ?
		 WHERE project_id = ? AND task_id = ?`,
		now, projectID, taskID,
	); err != nil {
		return nil, fmt.Errorf("quest: forfeit: update: %w", err)
	}

	if err := emitEvent(ctx, conn, projectID, taskID, EventReleased, owner, note, now); err != nil {
		return nil, err
	}

	result, err := loadTx(ctx, conn, projectID, taskID)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("quest: forfeit: commit: %w", err)
	}
	committed = true
	return &ForfeitResult{Quest: result}, nil
}
