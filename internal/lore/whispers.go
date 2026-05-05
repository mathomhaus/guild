package lore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Whispers returns `kind=idea` entries for project in the seed /
// exploring status window — the idea pipeline surface. topic is an
// optional filter; empty string means all topics.
//
// Only statuses 'seed' and 'exploring' are returned; `promoted` and
// `parked` ideas are considered out of the active pipeline.
func Whispers(ctx context.Context, db *sql.DB, project, topic string) ([]*Entry, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: whispers: nil db")
	}
	if strings.TrimSpace(project) == "" {
		return nil, fmt.Errorf("lore: whispers: project required")
	}

	clauses := []string{
		"e.project_id = ?",
		"e.kind = 'idea'",
		"e.status IN ('seed','exploring')",
	}
	args := []any{project}
	if topic != "" {
		clauses = append(clauses, "e.topic = ?")
		args = append(args, topic)
	}

	//nolint:gosec // G202: entryColumns + clauses are hard-coded fragments; user values reach SQL only via args
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY e.created_at DESC, e.id DESC`
	rows, err := db.QueryContext(ctx, sqlText, args...) //sqlcheck:ignore // sqlText is a constant template; clauses are hard-coded fragments
	if err != nil {
		return nil, fmt.Errorf("lore: whispers: query: %w", err)
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
		return nil, fmt.Errorf("lore: whispers: iterate: %w", err)
	}
	return out, nil
}
