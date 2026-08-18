package lore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Oath returns every `kind=principle` entry with status=current for
// project, sorted created_at DESC (newest first — the common reading
// order for the session-start oath wall). The reverse-chrono order
// surfaces the most recently added principles at the top of the feed.
//
// `e.id DESC` is the stable tie-breaker for entries written within the
// same SQLite-second; without it back-to-back inscribes can reorder
// non-deterministically across reads.
func Oath(ctx context.Context, db *sql.DB, project string) ([]*Entry, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: oath: nil db")
	}
	if strings.TrimSpace(project) == "" {
		return nil, fmt.Errorf("lore: oath: project required")
	}

	//nolint:gosec // G202: entryColumns is a constant; no user input reaches the SQL text
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE e.project_id = ? AND e.kind = 'principle' AND e.status = 'current'
		ORDER BY e.created_at DESC, e.id DESC`
	rows, err := db.QueryContext(ctx, sqlText, project) //sqlcheck:ignore // sqlText is a constant template; entryColumns is a constant
	if err != nil {
		return nil, fmt.Errorf("lore: oath: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Entry
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: oath: iterate: %w", err)
	}
	return out, nil
}
