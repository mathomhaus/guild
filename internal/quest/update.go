package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Update applies params to taskID. Semantics:
//
//  1. Scalar fields (Subject, Priority, Epic, Effort) OVERWRITE — last
//     value wins during replay.
//  2. List fields (Files, Acceptance, DependsOn, Blocks) APPEND by
//     default. The ReplaceX / ClearX variants cause full replacement.
//  3. Setting both the append form AND the replace form of the same
//     field in one call returns ErrConflictingUpdate.
//  4. If any new dep is not yet done AND the task isn't done/blocked,
//     the task auto-flips to 'blocked'.
//  5. ErrNoChange on an empty params.
//
// The spec-note wire format:
//
//   - Append scalars + non-acceptance list appends collapse into ONE
//     `[spec] k: v; k: v; ...` note.
//   - Each acceptance APPEND is one `[spec] acceptance: <crit>` note
//     (preserves commas inside a criterion).
//   - Replace scalars-free: one `[spec-replace] k: v; ...` note.
//   - Replace acceptance: first criterion → `[spec-replace] acceptance: <C0>`;
//     subsequent → `[spec] acceptance: <Ci>` (the replay semantics
//     reset-then-append collapse to "list becomes exactly [C0, C1, ...]").
//   - ClearFiles/ClearAcceptance/ClearDependsOn/ClearBlocks write a
//     `[spec-replace] <field>: ` note (empty value → empty list).
//
//nolint:gocritic // hugeParam: by-value keeps the API ergonomic for callers
func Update(ctx context.Context, db *sql.DB, projectID, taskID string, params UpdateParams) (*Quest, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: update: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: update: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return nil, fmt.Errorf("quest: update: empty task_id")
	}
	if params.Empty() {
		return nil, ErrNoChange
	}

	// Conflict check: can't have BOTH append and replace for the same
	// field in the same call. Accept Clear* as the replace form for
	// this check (since it's a degenerate replace to []).
	var conflicts []string
	if len(params.Files) > 0 && (len(params.ReplaceFiles) > 0 || params.ClearFiles) {
		conflicts = append(conflicts, "files")
	}
	if len(params.Acceptance) > 0 && (len(params.ReplaceAcceptance) > 0 || params.ClearAcceptance) {
		conflicts = append(conflicts, "acceptance")
	}
	if len(params.DependsOn) > 0 && (len(params.ReplaceDependsOn) > 0 || params.ClearDependsOn) {
		conflicts = append(conflicts, "depends_on")
	}
	if len(params.Blocks) > 0 && (len(params.ReplaceBlocks) > 0 || params.ClearBlocks) {
		conflicts = append(conflicts, "blocks")
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrConflictingUpdate, conflicts[0])
	}

	agent := agentOrDefault(params.Agent)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// BEGIN IMMEDIATE serializes concurrent Update calls: reads current status,
	// computes the new state, then writes — a stale DEFERRED snapshot could
	// cause two goroutines to read the same status and overwrite each other.
	conn, rollback, err := beginImmediate(ctx, db, "update")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// Exists check.
	var existStatus sql.NullString
	err = conn.QueryRowContext(ctx,
		`SELECT status FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&existStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, taskID)
		}
		return nil, fmt.Errorf("quest: update: probe existing: %w", err)
	}
	curStatus := Status(existStatus.String)

	// Accumulate the append-scalar-and-non-acceptance-list payload into
	// one [spec] note so read-time replay applies them atomically.
	var appendParts []string
	if v := strings.TrimSpace(params.Subject); v != "" {
		appendParts = append(appendParts, "subject: "+encodeSpecValue(v))
	}
	if v := strings.TrimSpace(string(params.Priority)); v != "" {
		appendParts = append(appendParts, "priority: "+v)
	}
	if v := strings.TrimSpace(params.Epic); v != "" {
		appendParts = append(appendParts, "epic: "+encodeSpecValue(v))
	}
	if v := strings.TrimSpace(params.Effort); v != "" {
		appendParts = append(appendParts, "effort: "+encodeSpecValue(v))
	}

	var newDepIDs []string
	for _, d := range params.DependsOn {
		d = strings.ToUpper(strings.TrimSpace(d))
		if d != "" {
			newDepIDs = append(newDepIDs, d)
		}
	}
	if len(newDepIDs) > 0 {
		appendParts = append(appendParts, "depends_on: "+strings.Join(newDepIDs, ", "))
	}
	if len(params.Blocks) > 0 {
		var bs []string
		for _, b := range params.Blocks {
			b = strings.ToUpper(strings.TrimSpace(b))
			if b != "" {
				bs = append(bs, b)
			}
		}
		if len(bs) > 0 {
			appendParts = append(appendParts, "blocks: "+strings.Join(bs, ", "))
		}
	}
	if len(params.Files) > 0 {
		if js := joinStrip(params.Files); js != "" {
			appendParts = append(appendParts, "files: "+js)
		}
	}

	if len(appendParts) > 0 {
		if err := insertSpecNote(ctx, conn, projectID, taskID, agent, now,
			NotePrefixSpec+strings.Join(appendParts, "; ")); err != nil {
			return nil, err
		}
	}

	// Replace scalars-free.
	var replaceParts []string
	var replaceDepIDs []string
	if len(params.ReplaceDependsOn) > 0 || params.ClearDependsOn {
		for _, d := range params.ReplaceDependsOn {
			d = strings.ToUpper(strings.TrimSpace(d))
			if d != "" {
				replaceDepIDs = append(replaceDepIDs, d)
			}
		}
		replaceParts = append(replaceParts, "depends_on: "+strings.Join(replaceDepIDs, ", "))
	}
	if len(params.ReplaceBlocks) > 0 || params.ClearBlocks {
		var bs []string
		for _, b := range params.ReplaceBlocks {
			b = strings.ToUpper(strings.TrimSpace(b))
			if b != "" {
				bs = append(bs, b)
			}
		}
		replaceParts = append(replaceParts, "blocks: "+strings.Join(bs, ", "))
	}
	if len(params.ReplaceFiles) > 0 || params.ClearFiles {
		replaceParts = append(replaceParts, "files: "+joinStrip(params.ReplaceFiles))
	}
	if len(replaceParts) > 0 {
		if err := insertSpecNote(ctx, conn, projectID, taskID, agent, now,
			NotePrefixSpecReplace+strings.Join(replaceParts, "; ")); err != nil {
			return nil, err
		}
	}

	// Acceptance — special-case because of per-criterion encoding.
	if len(params.ReplaceAcceptance) > 0 || params.ClearAcceptance {
		crits := make([]string, 0, len(params.ReplaceAcceptance))
		for _, c := range params.ReplaceAcceptance {
			c = strings.TrimSpace(c)
			if c != "" {
				crits = append(crits, c)
			}
		}
		if len(crits) == 0 {
			// Clear: emit one [spec-replace] with empty value so replay
			// resets to [].
			if err := insertSpecNote(ctx, conn, projectID, taskID, agent, now,
				NotePrefixSpecReplace+"acceptance: "); err != nil {
				return nil, err
			}
		} else {
			// First criterion [spec-replace] resets; rest [spec] append.
			if err := insertSpecNote(ctx, conn, projectID, taskID, agent, now,
				NotePrefixSpecReplace+"acceptance: "+encodeSpecValue(crits[0])); err != nil {
				return nil, err
			}
			for _, c := range crits[1:] {
				if err := insertSpecNote(ctx, conn, projectID, taskID, agent, now,
					NotePrefixSpec+"acceptance: "+encodeSpecValue(c)); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(params.Acceptance) > 0 {
		for _, c := range params.Acceptance {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if err := insertSpecNote(ctx, conn, projectID, taskID, agent, now,
				NotePrefixSpec+"acceptance: "+encodeSpecValue(c)); err != nil {
				return nil, err
			}
		}
	}

	// Recompute blockedness symmetrically when this call touched deps.
	// The auto-block half was original; the auto-unblock half fixes
	// QUEST-147 — clearing or replacing deps on a blocked quest used to
	// leave status='blocked' even after every dep was satisfied, stranding
	// the quest and tempting agents to raw-SQL unstick it.
	depsTouched := len(newDepIDs) > 0 ||
		len(params.ReplaceDependsOn) > 0 ||
		params.ClearDependsOn
	if depsTouched {
		if err := recomputeBlockedness(ctx, conn, projectID, taskID, curStatus, agent, now); err != nil {
			return nil, err
		}
	}

	// Bump updated_at so List/Scroll see the write order even when only
	// notes changed.
	if _, err := conn.ExecContext(ctx,
		`UPDATE task_status SET updated_at = ?
		 WHERE project_id = ? AND task_id = ?`,
		now, projectID, taskID,
	); err != nil {
		return nil, fmt.Errorf("quest: update: bump updated_at: %w", err)
	}

	if err := emitEvent(ctx, conn, projectID, taskID, "updated", agent, "", now); err != nil {
		return nil, err
	}

	result, err := loadTx(ctx, conn, projectID, taskID)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("quest: update: commit: %w", err)
	}
	committed = true
	return result, nil
}

// recomputeBlockedness flips taskID between 'blocked' and 'next' based
// on the canonical depends_on set after Update's spec notes have been
// written. Must be called AFTER the spec-note inserts so loadSpec
// observes the resulting dep list (append+replace+clear already
// applied).
//
// Transition table (curStatus → action):
//
//	next, in_progress → 'blocked' if any current dep is not done
//	blocked           → 'next'    if every current dep is done (or none)
//	done              → no-op (terminal)
//
// Only the status flip path that differs from the current state emits
// an event — a no-op recompute is silent. The blocked→next direction
// reuses EventUnblocked so pulse/scroll view the two unblock paths
// (Fulfill cascade + Update recompute) consistently.
//
// Cascade note: this helper does NOT walk `blocks` edges. An Update
// that unblocks B doesn't make B 'done', so no quest waiting on B's
// completion changes state. Cascade-through-done is Fulfill's job.
func recomputeBlockedness(ctx context.Context, tx dbTx, projectID, taskID string, curStatus Status, agent, now string) error {
	if curStatus == StatusDone {
		return nil
	}
	spec, err := loadSpec(ctx, tx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("quest: update: recompute deps: %w", err)
	}
	allDone, err := depsAllDone(ctx, tx, projectID, spec.DependsOn)
	if err != nil {
		return err
	}
	switch {
	case !allDone && curStatus != StatusBlocked:
		if _, err := tx.ExecContext(ctx,
			`UPDATE task_status
			 SET status = 'blocked', updated_at = ?
			 WHERE project_id = ? AND task_id = ?`,
			now, projectID, taskID,
		); err != nil {
			return fmt.Errorf("quest: update: auto-block: %w", err)
		}
		return emitEvent(ctx, tx, projectID, taskID, "blocked", agent,
			"blocked by: "+strings.Join(spec.DependsOn, ", "), now)
	case allDone && curStatus == StatusBlocked:
		if _, err := tx.ExecContext(ctx,
			`UPDATE task_status
			 SET status = 'next', updated_at = ?
			 WHERE project_id = ? AND task_id = ? AND status = 'blocked'`,
			now, projectID, taskID,
		); err != nil {
			return fmt.Errorf("quest: update: auto-unblock: %w", err)
		}
		return emitEvent(ctx, tx, projectID, taskID, EventUnblocked, agent,
			"deps satisfied", now)
	}
	return nil
}
