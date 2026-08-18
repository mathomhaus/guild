package lore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ListFilters is the filter surface for `lore list`. All fields are
// optional; empty strings mean "no filter". Status is special: when
// unset, we default to "NOT IN ('archived','superseded')" to expose
// only actively-maintained entries.
type ListFilters struct {
	Project  string
	Topic    string
	Kind     Kind
	Status   Status
	FilePath string
}

// List returns every entry matching the supplied filters, sorted the
// way the CLI prints them: kind ASC, then created_at DESC, with
// e.id DESC as a stable tie-breaker so same-second inserts have
// deterministic newest-first ordering across reads.
//
// Project="" + AllProjects is not supported — List always scopes by
// project. For a cross-project dump, callers can iterate project.List
// first and call List per project.
//
//nolint:gocritic // hugeParam: ListFilters is the public API surface; value semantics let callers build one inline without a temporary
func List(ctx context.Context, db *sql.DB, filters ListFilters) ([]*Entry, error) {
	return listWithFilters(ctx, db, &filters)
}

func listWithFilters(ctx context.Context, db *sql.DB, filters *ListFilters) ([]*Entry, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: list: nil db")
	}
	if strings.TrimSpace(filters.Project) == "" {
		return nil, fmt.Errorf("lore: list: project required")
	}

	var clauses []string
	var args []any

	clauses = append(clauses, "e.project_id = ?")
	args = append(args, filters.Project)

	if filters.Topic != "" {
		clauses = append(clauses, "e.topic = ?")
		args = append(args, filters.Topic)
	}
	if filters.Kind != "" {
		clauses = append(clauses, "e.kind = ?")
		args = append(args, string(filters.Kind))
	}
	if filters.Status != "" {
		clauses = append(clauses, "e.status = ?")
		args = append(args, string(filters.Status))
	} else {
		clauses = append(clauses, "e.status NOT IN ('archived','superseded')")
	}
	if filters.FilePath != "" {
		// Substring match across file_path, summary, and tags so prose
		// mentions count too.
		like := "%" + filters.FilePath + "%"
		clauses = append(clauses,
			"(e.file_path LIKE ? OR e.summary LIKE ? OR COALESCE(e.tags,'') LIKE ?)")
		args = append(args, like, like, like)
	}

	//nolint:gosec // G202: entryColumns + clauses are built from hard-coded fragments; user values reach SQL only via args
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY e.kind ASC, e.created_at DESC, e.id DESC`

	rows, err := db.QueryContext(ctx, sqlText, args...) //sqlcheck:ignore // sqlText is a constant template; clauses are hard-coded fragments; values reach SQL via args
	if err != nil {
		return nil, fmt.Errorf("lore: list: query: %w", err)
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
		return nil, fmt.Errorf("lore: list: iterate: %w", err)
	}
	return out, nil
}
