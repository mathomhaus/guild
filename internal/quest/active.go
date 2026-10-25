package quest

import (
	"context"
	"database/sql"
	"fmt"
)

// Active returns all quests currently in status='in_progress' across
// every project registered in the DB, or only in projectID when it is
// non-empty. The result is sorted by project_id ascending, then
// claimed_at ascending (oldest first within each project).
func Active(ctx context.Context, db *sql.DB, projectID string) ([]*Quest, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: active: nil db")
	}

	rows, err := db.QueryContext(ctx,
		`SELECT project_id, task_id FROM task_status
		 WHERE status = 'in_progress' AND (? = '' OR project_id = ?)
		 ORDER BY project_id ASC, claimed_at ASC`,
		projectID, projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: active: query: %w", err)
	}

	type pair struct {
		projectID string
		taskID    string
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.projectID, &p.taskID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("quest: active: scan: %w", err)
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: active: iterate: %w", err)
	}
	_ = rows.Close()

	// Resolve full Quest for each in-progress row.
	out := make([]*Quest, 0, len(pairs))
	for _, p := range pairs {
		q, err := Load(ctx, db, p.projectID, p.taskID)
		if err != nil {
			// Task disappeared between query and load (race or delete).
			continue
		}
		out = append(out, q)
	}
	return out, nil
}
