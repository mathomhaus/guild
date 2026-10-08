package lore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Persistent staleness signals.
//
// lore.Echoes recomputes staleness on every call: a valid_days check per
// entry, plus (gitAware=true) one git subprocess per entry with a
// file_path. A long-lived observer (file watcher, scheduled sweep)
// cannot afford to re-derive that on every read, so this file provides
// the durable write side: signals persisted into the staleness_signals
// table (migration 010), keyed (entry_id, source) so repeated
// observations upsert instead of piling up.
//
// Invalidation rule: signals are read-side-invalidated. Echoes only
// surfaces a signal while its entry is still status='current'; the
// moment an entry leaves that status (reforge -> superseded, seal ->
// archived, project archive) every signal for it stops surfacing, with
// no bookkeeping write required on the transition. Rows for non-current
// entries are inert and harmless.

// Signal sources. The source distinguishes who observed the staleness;
// it is the second half of the (entry_id, source) upsert key, so each
// observer refreshes its own row instead of stacking duplicates.
const (
	// SourceWatcherFile marks signals written by a filesystem watcher
	// that saw the entry's file_path change on disk.
	SourceWatcherFile = "watcher-file"
	// SourceGitSweep marks signals written by the project-scoped git
	// sweep (GitSweep), which replays the git-aware echo check once and
	// persists the hits.
	SourceGitSweep = "git-sweep"
)

// reasonFileChanged is the display-ready reason FlagStaleByPath persists.
// Worded to match the query-time git reason's voice in echoReason.
const reasonFileChanged = "file changed after entry was created"

// FlagStaleByPath flags every status='current' entry in projectID whose
// file_path exactly matches absPath, persisting one staleness signal per
// entry under source, and returns the flagged entry IDs in ascending
// order. Re-flagging the same (entry, source) refreshes the existing
// signal row (reason + observed_at) rather than adding another.
//
// The read of matching entries and the signal upserts share one BEGIN
// IMMEDIATE transaction, so an entry that leaves status='current'
// concurrently is never flagged from a stale snapshot. Safe to call from
// any process; single-writer daemons are an optimization, not a
// correctness dependency.
func FlagStaleByPath(ctx context.Context, db *sql.DB, projectID, absPath, source string, now time.Time) ([]int64, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: flag stale by path: nil db")
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("lore: flag stale by path: project required")
	}
	if strings.TrimSpace(absPath) == "" {
		return nil, fmt.Errorf("lore: flag stale by path: path required")
	}
	if strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("lore: flag stale by path: source required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	conn, rollback, err := beginImmediate(ctx, db, "lore: flag stale by path")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	committed := false
	defer rollback(&committed)

	rows, err := conn.QueryContext(ctx,
		`SELECT id FROM entries
		  WHERE project_id = ? AND status = 'current' AND file_path = ?
		  ORDER BY id ASC`,
		projectID, absPath,
	)
	if err != nil {
		return nil, fmt.Errorf("lore: flag stale by path: query: %w", err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("lore: flag stale by path: %w", err)
	}
	if len(ids) == 0 {
		// Nothing to flag; commit the empty transaction so the
		// IMMEDIATE lock releases cleanly.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, fmt.Errorf("lore: flag stale by path: commit: %w", err)
		}
		committed = true
		return nil, nil
	}

	observedAt := now.Format(time.RFC3339)
	for _, id := range ids {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO staleness_signals (entry_id, project_id, reason, source, observed_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(entry_id, source) DO UPDATE SET
			   reason = excluded.reason,
			   observed_at = excluded.observed_at`,
			id, projectID, reasonFileChanged, source, observedAt,
		); err != nil {
			return nil, fmt.Errorf("lore: flag stale by path: upsert signal for entry %d: %w", id, err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("lore: flag stale by path: commit: %w", err)
	}
	committed = true
	return ids, nil
}

// GitSweep replays the git-aware echo check once for every
// status='current' entry in projectID that carries a file_path, and
// persists a SourceGitSweep signal for each hit (file modified in git
// after the entry was created). Returns the flagged entry IDs in
// ascending order. Repeated sweeps refresh existing rows via the
// (entry_id, source) upsert.
//
// It reuses the echoes.go machinery: the same per-batch repoRootResolver
// cache (N entries in one repo cost N+1 subprocesses, not 2N) and the
// same gitFileLastModified seam, so tests and future behavior changes
// stay in one place. Git failures degrade silently per entry, exactly
// like the query-time check.
//
// The git subprocess pass runs outside any transaction (it can take
// seconds); each upsert re-checks status='current' so an entry retired
// mid-sweep is not flagged.
func GitSweep(ctx context.Context, db *sql.DB, projectID string, now time.Time) ([]int64, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: git sweep: nil db")
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("lore: git sweep: project required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	//nolint:gosec // G202: entryColumns is a constant; no user input reaches the SQL text
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE e.project_id = ? AND e.status = 'current' AND COALESCE(e.file_path,'') != ''
		ORDER BY e.id ASC`
	rows, err := db.QueryContext(ctx, sqlText, projectID) //sqlcheck:ignore // sqlText is a constant template; entryColumns is a constant
	if err != nil {
		return nil, fmt.Errorf("lore: git sweep: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Same per-batch cache Echoes installs; lives only for this sweep.
	ctx = withRepoRootResolver(ctx, newRepoRootResolver())

	var hits []int64
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		if gitDate := gitFileLastModified(ctx, e.FilePath); !gitDate.IsZero() && gitDate.After(e.CreatedAt) {
			hits = append(hits, e.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: git sweep: iterate: %w", err)
	}
	if len(hits) == 0 {
		return nil, nil
	}

	observedAt := now.Format(time.RFC3339)
	var flagged []int64
	for _, id := range hits {
		res, err := db.ExecContext(ctx,
			`INSERT INTO staleness_signals (entry_id, project_id, reason, source, observed_at)
			 SELECT id, project_id, ?, ?, ?
			   FROM entries WHERE id = ? AND status = 'current'
			 ON CONFLICT(entry_id, source) DO UPDATE SET
			   reason = excluded.reason,
			   observed_at = excluded.observed_at`,
			reasonFileChanged, SourceGitSweep, observedAt, id,
		)
		if err != nil {
			return nil, fmt.Errorf("lore: git sweep: upsert signal for entry %d: %w", id, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			flagged = append(flagged, id)
		}
	}
	sort.Slice(flagged, func(i, j int) bool { return flagged[i] < flagged[j] })
	return flagged, nil
}

// loadStalenessSignals returns the persisted signal reasons for one
// project, keyed by entry ID. Entries with signals from multiple sources
// get their reasons joined with "; " in deterministic source order.
// Echoes unions this map into its scan of current entries; because the
// union is keyed off that scan, signals for non-current entries never
// surface (the read-side invalidation rule).
func loadStalenessSignals(ctx context.Context, db *sql.DB, projectID string) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT entry_id, reason FROM staleness_signals
		  WHERE project_id = ?
		  ORDER BY entry_id ASC, source ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("lore: load staleness signals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	signals := map[int64]string{}
	for rows.Next() {
		var id int64
		var reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, fmt.Errorf("lore: load staleness signals: scan: %w", err)
		}
		if prev, ok := signals[id]; ok && prev != reason {
			signals[id] = prev + "; " + reason
		} else {
			signals[id] = reason
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: load staleness signals: iterate: %w", err)
	}
	return signals, nil
}

// collectIDs drains a single-int64-column row cursor.
func collectIDs(rows *sql.Rows) ([]int64, error) {
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ids: %w", err)
	}
	return ids, nil
}
