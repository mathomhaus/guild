package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Seal marks entry id as archived in ProjectID's scope. Returns the
// post-seal Entry so callers can print the updated status.
//
// Sealing is idempotent: sealing an already-archived entry succeeds but
// bumps updated_at. We intentionally don't guard against that to simplify
// the CLI surface (no "entry already sealed" error-case branch).
//
// Per ADR-003 "Mutation semantics", a seal that flips a row from a
// coverage-eligible status (not archived, not parked) to 'archived'
// must decrement meta.vector_coverage_den so the denominator matches
// the set of entries eligible for embedding. The decrement happens in
// the same BEGIN IMMEDIATE as the status flip so the ratio is always
// atomic relative to concurrent appraise calls.
func Seal(ctx context.Context, db *sql.DB, id int64, projectID string, now time.Time) (*Entry, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: seal: nil db")
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("%w: project id", ErrMissingField)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	conn, rollback, err := beginImmediate(ctx, db, "lore: seal")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	committed := false
	defer rollback(&committed)

	// Read pre-state so we can tell whether this seal actually
	// transitions from "in coverage" to "out of coverage." Re-sealing
	// an already-archived row must not decrement den twice.
	var preStatus string
	err = conn.QueryRowContext(ctx,
		`SELECT status FROM entries WHERE id = ? AND project_id = ?`,
		id, projectID,
	).Scan(&preStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s (project %s)",
				ErrEntryNotFound, EntryID(id), projectID)
		}
		return nil, fmt.Errorf("lore: seal: read current status: %w", err)
	}

	res, err := conn.ExecContext(ctx,
		`UPDATE entries
		   SET status = 'archived', updated_at = ?
		 WHERE id = ? AND project_id = ?`,
		now.Format(time.RFC3339), id, projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("lore: seal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("lore: seal: rows affected: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: %s (project %s)", ErrEntryNotFound, EntryID(id), projectID)
	}

	// Decrement den only when we actually transitioned from a
	// coverage-eligible status to archived. Mirrors migration 003's
	// seeding WHERE status NOT IN ('archived', 'parked').
	if preStatus != string(StatusArchived) && preStatus != string(StatusParked) {
		if _, err := conn.ExecContext(ctx, sqlDecrCoverageDen); err != nil {
			return nil, fmt.Errorf("lore: seal: decrement vector_coverage_den: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("lore: seal: commit: %w", err)
	}
	committed = true

	return loadEntry(ctx, db, id)
}
