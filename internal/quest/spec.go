package quest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Note prefix constants for the spec event log live in events.go. This
// file consumes NotePrefixSpec / NotePrefixSpecReplace / NotePrefixRework
// via replay.

// specListFields are the keys whose values are list-valued in the spec
// replay. Appended by [spec], reset-then-applied by [spec-replace].
var specListFields = map[string]bool{
	"files":      true,
	"acceptance": true,
	"depends_on": true,
	"blocks":     true,
}

// sqlLoadSpec is the SELECT used by loadSpec. Composed once at init so
// every Scroll/List/Bounties call reuses the same string rather than
// re-running fmt.Sprintf per call. The constant formatters are derived
// from events.go so writer and reader remain structurally linked.
var sqlLoadSpec = fmt.Sprintf(`
	SELECT note
	FROM task_notes
	WHERE project_id = ? AND task_id = ?
	  AND (note LIKE '%s%%' OR note LIKE '%s%%' OR note LIKE '%s%%')
	ORDER BY id ASC`, NotePrefixSpec, NotePrefixSpecReplace, NotePrefixRework)

// loadSpec replays every [spec] / [spec-replace] / [rework] note for
// taskID in the given project, returning the resolved Quest fields
// (subject/priority/epic/effort/files/acceptance/depends_on/blocks/rework_of)
// derived from the event log.
//
// Replay rules:
//   - [spec] notes: scalar fields last-value-wins; list fields append.
//   - [spec-replace] notes: scalar still last-value-wins; list fields
//     reset BEFORE the payload is applied on this note.
//   - [rework] of: stored in ReworkOf (most recent wins).
//
// The acceptance-criterion-per-note encoding: each criterion is a
// separate "[spec] acceptance: <one criterion>" note. On read, each such
// note contributes exactly one item, so commas inside a criterion are
// preserved. "[spec-replace] acceptance: X" + "[spec] acceptance: Y"
// yields [X, Y] — first note resets, subsequent notes append.
//
// Returns zero-value Quest (no error) if taskID has no spec notes yet —
// callers should combine with the task_status row to build a complete
// Quest.
func loadSpec(ctx context.Context, q queryer, projectID, taskID string) (*Quest, error) {
	rows, err := q.QueryContext(ctx, sqlLoadSpec, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("quest: load spec %s/%s: %w", projectID, taskID, err)
	}
	defer func() { _ = rows.Close() }()

	qst := &Quest{ID: taskID}
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return nil, fmt.Errorf("quest: scan spec note %s/%s: %w", projectID, taskID, err)
		}
		applyNote(qst, note)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: iterate spec notes %s/%s: %w", projectID, taskID, err)
	}
	return qst, nil
}

// applyNote applies one [spec]/[spec-replace]/[rework] note to q.
// Extracted so tests can exercise replay without a DB. Unknown prefixes
// are silently ignored — forward-compat with prefixes a newer writer
// might introduce.
func applyNote(q *Quest, note string) {
	switch {
	case strings.HasPrefix(note, NotePrefixRework):
		q.ReworkOf = strings.TrimSpace(note[len(NotePrefixRework):])
		return
	case strings.HasPrefix(note, NotePrefixSpec):
		applyPayload(q, note[len(NotePrefixSpec):], false)
	case strings.HasPrefix(note, NotePrefixSpecReplace):
		applyPayload(q, note[len(NotePrefixSpecReplace):], true)
	}
}

// applyPayload parses one "k: v; k: v; ..." payload and merges into q.
// replace=true means list fields reset BEFORE append. Scalar fields are
// last-value-wins regardless of replace mode.
func applyPayload(q *Quest, payload string, replace bool) {
	for _, part := range splitSpecParts(payload) {
		k, v, ok := splitKV(part)
		if !ok {
			continue
		}
		if specListFields[k] {
			var items []string
			if k == "acceptance" {
				// Each acceptance note carries exactly one criterion —
				// don't comma-split the value. Preserves boundaries.
				if v != "" {
					items = []string{decodeSpecValue(v)}
				}
			} else {
				items = splitCommaList(v)
			}
			assignListField(q, k, items, replace)
			continue
		}
		assignScalarField(q, k, v)
	}
}

func splitSpecParts(payload string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(payload)-1; i++ {
		if payload[i] != ';' || payload[i+1] != ' ' {
			continue
		}
		if !startsSpecField(payload[i+2:]) {
			continue
		}
		parts = append(parts, payload[start:i])
		start = i + 2
		i++
	}
	parts = append(parts, payload[start:])
	return parts
}

func startsSpecField(s string) bool {
	for _, key := range []string{"subject", "priority", "epic", "effort", "files", "acceptance", "depends_on", "blocks"} {
		if strings.HasPrefix(s, key+":") {
			return true
		}
	}
	return false
}

// splitKV splits "key: value" into (key, value, ok). Returns ok=false
// on blank keys or missing colons.
func splitKV(part string) (key, value string, ok bool) {
	part = strings.TrimSpace(part)
	if part == "" {
		return "", "", false
	}
	idx := strings.Index(part, ": ")
	if idx < 0 {
		// Tolerate "k:v" with no space (older notes).
		if c := strings.Index(part, ":"); c >= 0 {
			return strings.TrimSpace(part[:c]), strings.TrimSpace(part[c+1:]), true
		}
		return "", "", false
	}
	k := strings.TrimSpace(part[:idx])
	v := strings.TrimSpace(part[idx+2:])
	if k == "" {
		return "", "", false
	}
	return k, v, true
}

// splitCommaList splits "a, b, c" → ["a", "b", "c"], trimming whitespace
// around each item and dropping empties.
func splitCommaList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, decodeSpecValue(p))
		}
	}
	return out
}

func assignScalarField(q *Quest, k, v string) {
	v = decodeSpecValue(v)
	switch k {
	case "subject":
		q.Subject = v
	case "priority":
		q.Priority = Priority(v)
	case "epic":
		q.Epic = v
	case "effort":
		q.Effort = v
	}
}

const specEncodedPrefix = "url:"

func encodeSpecValue(v string) string {
	return specEncodedPrefix + url.QueryEscape(v)
}

func decodeSpecValue(v string) string {
	if !strings.HasPrefix(v, specEncodedPrefix) {
		return v
	}
	decoded, err := url.QueryUnescape(strings.TrimPrefix(v, specEncodedPrefix))
	if err != nil {
		return v
	}
	return decoded
}

func assignListField(q *Quest, k string, items []string, replace bool) {
	switch k {
	case "files":
		if replace {
			q.Files = items
		} else {
			q.Files = append(q.Files, items...)
		}
	case "acceptance":
		if replace {
			q.Acceptance = items
		} else {
			q.Acceptance = append(q.Acceptance, items...)
		}
	case "depends_on":
		if replace {
			q.DependsOn = upperAll(items)
		} else {
			q.DependsOn = append(q.DependsOn, upperAll(items)...)
		}
	case "blocks":
		if replace {
			q.Blocks = upperAll(items)
		} else {
			q.Blocks = append(q.Blocks, upperAll(items)...)
		}
	}
}

// upperAll returns a copy of xs with every element uppercased. Used for
// depends_on / blocks because quest IDs are canonically uppercase
// ("QUEST-7") and writes normalize to uppercase.
func upperAll(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strings.ToUpper(strings.TrimSpace(x))
	}
	return out
}

// overlayStatus fills in the status/claimed_by/claimed_at fields on q by
// looking up the task_status row. Called by Load to build a complete
// Quest.
func overlayStatus(ctx context.Context, q queryer, projectID, taskID string, out *Quest) error {
	var status, claimedBy, claimedAt, updatedAt sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT status, claimed_by, claimed_at, updated_at
		 FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&status, &claimedBy, &claimedAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %s", ErrNotFound, taskID)
		}
		return fmt.Errorf("quest: overlay status %s/%s: %w", projectID, taskID, err)
	}
	out.Status = Status(status.String)
	out.Owner = claimedBy.String
	if claimedAt.Valid && claimedAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, claimedAt.String); err == nil {
			out.ClaimedAt = &t
		} else if t, err := time.Parse(time.RFC3339, claimedAt.String); err == nil {
			out.ClaimedAt = &t
		}
	}
	if updatedAt.Valid && updatedAt.String != "" {
		if t, err := parseFlexibleTime(updatedAt.String); err == nil {
			out.UpdatedAt = &t
		}
	}
	return nil
}

// parseFlexibleTime accepts both RFC3339 and SQLite's datetime('now')
// output ("2006-01-02 15:04:05"), which appear in task_status.updated_at
// depending on which write path created the row.
func parseFlexibleTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("quest: unrecognized time format %q", s)
}

// Load returns the fully-resolved Quest for taskID in projectID,
// combining event-sourced spec fields with the task_status row. Returns
// ErrNotFound if the quest doesn't exist in task_status.
//
// Load is used by callers who need a complete Quest after a write —
// notably the CLI layer for formatting emoji lines and the MCP wrapper
// for JSON responses.
func Load(ctx context.Context, db *sql.DB, projectID, taskID string) (*Quest, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: load: nil db")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return nil, fmt.Errorf("quest: load: empty task_id")
	}
	q, err := loadSpec(ctx, db, projectID, taskID)
	if err != nil {
		return nil, err
	}
	if err := overlayStatus(ctx, db, projectID, taskID, q); err != nil {
		return nil, err
	}
	return q, nil
}

// loadTx is the transaction-bound form of Load used by Post/Accept/etc
// after a write so the returned Quest reflects the in-tx state, not a
// racing view of the db.
func loadTx(ctx context.Context, tx queryer, projectID, taskID string) (*Quest, error) {
	q, err := loadSpec(ctx, tx, projectID, taskID)
	if err != nil {
		return nil, err
	}
	if err := overlayStatus(ctx, tx, projectID, taskID, q); err != nil {
		return nil, err
	}
	return q, nil
}

// queryer is the subset of *sql.DB / *sql.Tx our spec-replay needs. Lets
// us reuse loadSpec inside transactions without type assertions.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dbTx is the read+write subset of *sql.Tx and *sql.Conn. Accepting this
// interface instead of *sql.Tx lets Fulfill pin to a *sql.Conn and issue
// BEGIN IMMEDIATE directly, without changing the cascade-helper signatures
// visible to other callers (which continue to pass *sql.Tx and satisfy the
// interface implicitly).
type dbTx interface {
	queryer
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
