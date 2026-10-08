package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// questIDRe matches "QUEST-NNN" at end of a task_id. Used to pick the
// next monotonic id when Post mints a new quest.
var questIDRe = regexp.MustCompile(`^QUEST-(\d+)$`)

// Post creates a new quest in projectID.
//
// Semantics:
//
//  1. Pick the next monotonic id: scan every task_status row in the
//     project, extract the max numeric suffix, emit "QUEST-<max+1>".
//  2. Insert a task_status row with status='next' (or 'blocked' if deps
//     not all done, see step 5).
//  3. Write one combined [spec] note for the scalar fields that are set
//     (subject: ...; priority: ...; epic: ...; effort: ...).
//  4. Write a separate [spec] note for each non-empty list field:
//     files=comma-joined, depends_on=comma-joined uppercased, but
//     acceptance gets ONE NOTE PER CRITERION to preserve internal
//     commas.
//  5. If DependsOn is non-empty, check whether all cited quests are in
//     status=done. If not, flip the new row to status=blocked so it
//     shows up in `cascade-unblock` when the last dep clears.
//  6. If ReworkOf is set, write a [rework] of: note.
//  7. Emit a `created` event into task_events for pulse/scroll.
//
// All writes happen in one transaction so a mid-write failure doesn't
// leave a task_status row with no [spec] notes.
//
//nolint:gocritic // hugeParam: by-value keeps the API ergonomic for callers
func Post(ctx context.Context, db *sql.DB, projectID string, params PostParams) (*Quest, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: post: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: post: empty project_id")
	}
	if strings.TrimSpace(params.Subject) == "" {
		return nil, fmt.Errorf("quest: post: empty subject")
	}

	agent := agentOrDefault(params.Agent)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// BEGIN IMMEDIATE serializes concurrent Post calls so nextQuestID's
	// MAX(id) scan and the subsequent INSERT see the same committed state
	// (DEFERRED would read a stale snapshot and could produce duplicate IDs
	// under contention, relying on the PK to reject the second writer).
	conn, rollback, err := beginImmediate(ctx, db, "post")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	result, err := postTx(ctx, conn, projectID, &params, agent, now)
	if err != nil {
		return nil, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("quest: post: commit: %w", err)
	}
	committed = true
	return result, nil
}

// postTx runs the full Post write sequence inside an already-open write
// transaction on tx: monotonic id minting, the task_status insert, the
// [spec] notes, the blocked-status dependency check, and the `created`
// event. Extracted from Post so batch posters (e.g. PostRenewals) can
// compose extra writes (such as dedupe marker notes) into the same
// transaction. The caller owns BEGIN/COMMIT/ROLLBACK and is responsible
// for validating params before calling.
func postTx(ctx context.Context, tx dbTx, projectID string, params *PostParams, agent, now string) (*Quest, error) {
	newID, err := nextQuestID(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}

	// Normalize depends_on UPPERCASE; used for both the [spec] note
	// payload and the blocked-status decision below.
	var depIDs []string
	for _, d := range params.DependsOn {
		d = strings.ToUpper(strings.TrimSpace(d))
		if d != "" {
			depIDs = append(depIDs, d)
		}
	}

	initialStatus := StatusNext
	if len(depIDs) > 0 {
		allDone, err := depsAllDone(ctx, tx, projectID, depIDs)
		if err != nil {
			return nil, err
		}
		if !allDone {
			initialStatus = StatusBlocked
		}
	}

	// Insert task_status row.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_status (project_id, task_id, status, updated_at)
		 VALUES (?, ?, ?, ?)`,
		projectID, newID, string(initialStatus), now,
	); err != nil {
		return nil, fmt.Errorf("quest: post: insert task_status: %w", err)
	}

	// Combined [spec] for scalars. Subject is always present (validated
	// above); the rest are conditional.
	scalarParts := []string{fmt.Sprintf("subject: %s", params.Subject)}
	if p := strings.TrimSpace(string(params.Priority)); p != "" {
		scalarParts = append(scalarParts, "priority: "+p)
	}
	if e := strings.TrimSpace(params.Epic); e != "" {
		scalarParts = append(scalarParts, "epic: "+e)
	}
	if e := strings.TrimSpace(params.Effort); e != "" {
		scalarParts = append(scalarParts, "effort: "+e)
	}
	if err := insertSpecNote(ctx, tx, projectID, newID, agent, now,
		NotePrefixSpec+strings.Join(scalarParts, "; ")); err != nil {
		return nil, err
	}

	// List fields — separate note each. files/depends_on comma-join on
	// one line; acceptance is one note per criterion.
	if len(params.Files) > 0 {
		if err := insertSpecNote(ctx, tx, projectID, newID, agent, now,
			NotePrefixSpec+"files: "+joinStrip(params.Files)); err != nil {
			return nil, err
		}
	}
	if len(depIDs) > 0 {
		if err := insertSpecNote(ctx, tx, projectID, newID, agent, now,
			NotePrefixSpec+"depends_on: "+strings.Join(depIDs, ", ")); err != nil {
			return nil, err
		}
	}
	for _, crit := range params.Acceptance {
		crit = strings.TrimSpace(crit)
		if crit == "" {
			continue
		}
		if err := insertSpecNote(ctx, tx, projectID, newID, agent, now,
			NotePrefixSpec+"acceptance: "+crit); err != nil {
			return nil, err
		}
	}
	if rework := strings.TrimSpace(params.ReworkOf); rework != "" {
		rework = strings.ToUpper(rework)
		if err := insertSpecNote(ctx, tx, projectID, newID, agent, now,
			NotePrefixRework+rework); err != nil {
			return nil, err
		}
	}

	// `created` event (for scroll/pulse). data = subject.
	if err := emitEvent(ctx, tx, projectID, newID, EventCreated, agent, params.Subject, now); err != nil {
		return nil, err
	}

	return loadTx(ctx, tx, projectID, newID)
}

// nextQuestID returns "QUEST-<max+1>" where max is the highest numeric
// suffix in the project's task_status. Runs inside the caller's tx so
// concurrent Post calls in two goroutines can't pick the same id (the
// task_status PRIMARY KEY would reject the second anyway, but computing
// the id inside the tx also prevents a spurious error-before-commit).
func nextQuestID(ctx context.Context, tx dbTx, projectID string) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT task_id FROM task_status WHERE project_id = ?`,
		projectID,
	)
	if err != nil {
		return "", fmt.Errorf("quest: next id: %w", err)
	}
	defer func() { _ = rows.Close() }()

	highest := 0
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return "", fmt.Errorf("quest: next id scan: %w", err)
		}
		if m := questIDRe.FindStringSubmatch(tid); m != nil {
			n := 0
			for _, c := range m[1] {
				n = n*10 + int(c-'0')
			}
			if n > highest {
				highest = n
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("quest: next id iter: %w", err)
	}
	return fmt.Sprintf("QUEST-%d", highest+1), nil
}

// depsAllDone reports whether every id in depIDs has a task_status row
// with status='done' in projectID. A missing id counts as NOT done
// (a dep not yet posted doesn't unblock).
//
// Implementation: one SELECT per dep with parameterized placeholders.
// The slice is tiny in practice (<10 ids) so the query-per-id cost is
// negligible. Per-id queries also sidestep the dynamic IN-clause
// placeholder construction that sqlcheck flags as a possible
// SQL-injection vector, even though the only strings concatenated
// would be literal "?,"s. Cleaner to remove the flag entirely.
func depsAllDone(ctx context.Context, tx dbTx, projectID string, depIDs []string) (bool, error) {
	if len(depIDs) == 0 {
		return true, nil
	}
	const sqlQ = `SELECT 1 FROM task_status
	              WHERE project_id = ? AND task_id = ? AND status = 'done'`
	for _, d := range depIDs {
		var found int
		err := tx.QueryRowContext(ctx, sqlQ, projectID, d).Scan(&found)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, fmt.Errorf("quest: deps all done %s: %w", d, err)
		}
	}
	return true, nil
}

// insertSpecNote writes one row into task_notes with the given
// pre-assembled note string. Note must already include its system
// prefix ("[spec] ", "[spec-replace] ", "[rework] of: ",
// "[renewal] entry: ", ...).
func insertSpecNote(ctx context.Context, tx dbTx, projectID, taskID, agent, createdAt, note string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		projectID, taskID, agent, note, createdAt,
	)
	if err != nil {
		return fmt.Errorf("quest: insert spec note: %w", err)
	}
	return nil
}

// emitEvent writes one row into task_events.
func emitEvent(ctx context.Context, tx dbTx, projectID, taskID, event, agent, data, createdAt string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO task_events (project_id, task_id, event, agent_id, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, taskID, event, nullIfEmpty(agent), nullIfEmpty(data), createdAt,
	)
	if err != nil {
		return fmt.Errorf("quest: emit event: %w", err)
	}
	return nil
}

// nullIfEmpty converts "" → nil so empty fields land as NULL in the DB.
// Returned as any so it mixes into args slices cleanly.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// agentOrDefault returns agent if non-empty, else "agent". Callers
// that want env-derived agents pass them in.
func agentOrDefault(agent string) string {
	if strings.TrimSpace(agent) == "" {
		return "agent"
	}
	return agent
}

// joinStrip joins xs with ", ", trimming each item.
func joinStrip(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x != "" {
			parts = append(parts, x)
		}
	}
	return strings.Join(parts, ", ")
}
