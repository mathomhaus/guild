package quest

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PulseReport is the quality dashboard returned by Pulse.
//
// Pulse computes its report entirely from the quest DB (no git shelling out):
//   - ReworkRate: fraction of recently-cleared quests that are reworks
//     (have a [rework] of: note), matching the declared-rework signal.
//   - ChurnScore: median notes-per-quest for quests cleared in the window,
//     where notes = count of [spec] updates after the initial post. High
//     spec-churn means quests were reopened/renegotiated frequently.
//   - HotFiles: files appearing in the most quests (from Files field of specs).
//
// When the window contains no cleared quests, fields are zero/nil but the
// struct is still returned (not nil) so callers can print graceful output.
type PulseReport struct {
	ProjectID string

	// Window is the duration that was applied.
	Window time.Duration

	// ClearedTotal is the total quests cleared (all time).
	ClearedTotal int

	// ClearedInWindow is quests cleared within the window.
	ClearedInWindow int

	// ReworkCount is the number of window-cleared quests that are reworks.
	ReworkCount int

	// ReworkPct is ReworkCount/ClearedInWindow*100 (0 when no cleared quests).
	ReworkPct int

	// HighRework is true when ReworkPct >= 20.
	HighRework bool

	// ChurnMedian is the median spec-update count per quest in the window.
	// A [spec] note added after the initial post counts as a spec update.
	// 0 when no cleared quests in window.
	ChurnMedian float64

	// HotFiles lists files touched by 2+ quests, descending by quest count.
	// At most 3 entries.
	HotFiles []HotFile

	// UntrackedRework is quests with no rework_of but updated more than once
	// (a softer signal of untracked rework).
	UntrackedRework int

	// NoReport is the count of cleared quests with no [completed] note
	// (hint: pass --report on clear).
	NoReport int
}

// HotFile pairs a file path with the number of distinct quests that list it.
type HotFile struct {
	File       string
	QuestCount int
}

// ParseWindow parses a duration string used by --window. Accepted forms:
// "Nd" (N days), "Nw" (N weeks), "Nm" (N months ≈ 30 days).
// Falls back to numeric-only strings treated as days for backwards compat.
// Returns an error for unrecognized formats.
func ParseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 30 * 24 * time.Hour, nil
	}

	// Check if the last character is a unit letter.
	last := s[len(s)-1]
	if last == 'd' || last == 'D' || last == 'w' || last == 'W' || last == 'm' || last == 'M' {
		rest := s[:len(s)-1]
		var n int
		if _, err := fmt.Sscanf(rest, "%d", &n); err != nil || n <= 0 {
			return 0, fmt.Errorf("quest: pulse: invalid window %q (use Nd, Nw, or Nm)", s)
		}
		switch last | 0x20 { // lowercase
		case 'd':
			return time.Duration(n) * 24 * time.Hour, nil
		case 'w':
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		case 'm':
			return time.Duration(n) * 30 * 24 * time.Hour, nil
		}
	}

	// Pure numeric → days.
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		return time.Duration(n) * 24 * time.Hour, nil
	}

	return 0, fmt.Errorf("quest: pulse: invalid window %q (use Nd, Nw, or Nm)", s)
}

// Pulse computes the quest-quality dashboard for projectID over window.
// Default window: 30 days (pass 0 to use default).
func Pulse(ctx context.Context, db *sql.DB, projectID string, window time.Duration) (*PulseReport, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: pulse: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: pulse: empty project_id")
	}
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}

	report := &PulseReport{
		ProjectID: projectID,
		Window:    window,
	}

	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)

	// --- 1. Cleared quests (all time + window) ---
	rows, err := db.QueryContext(ctx,
		`SELECT task_id, updated_at FROM task_status
		 WHERE project_id = ? AND status = 'done'
		 ORDER BY updated_at DESC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: pulse: query cleared: %w", err)
	}

	type cleared struct {
		taskID    string
		updatedAt string
	}
	var allCleared []cleared
	for rows.Next() {
		var c cleared
		var updAt sql.NullString
		if err := rows.Scan(&c.taskID, &updAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("quest: pulse: scan cleared: %w", err)
		}
		c.updatedAt = updAt.String
		allCleared = append(allCleared, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: pulse: iterate cleared: %w", err)
	}
	_ = rows.Close()

	report.ClearedTotal = len(allCleared)

	// Filter to window.
	var windowCleared []cleared
	for _, c := range allCleared {
		// Compare lexicographically — both are RFC3339 or SQLite datetime
		// (YYYY-MM-DD HH:MM:SS) strings; both sort correctly as strings
		// for the same calendar period.
		if c.updatedAt >= cutoff || c.updatedAt == "" {
			windowCleared = append(windowCleared, c)
		}
	}
	report.ClearedInWindow = len(windowCleared)

	if len(windowCleared) == 0 {
		return report, nil
	}

	// Build set of window-cleared task IDs for fast lookup.
	windowIDs := make(map[string]bool, len(windowCleared))
	for _, c := range windowCleared {
		windowIDs[c.taskID] = true
	}

	// --- 2. Rework notes (all cleared quests — rework can pre-date window) ---
	reworkRows, err := db.QueryContext(ctx, //nolint:sqlcheck // NotePrefixRework is a package constant
		fmt.Sprintf(`SELECT task_id FROM task_notes
		 WHERE project_id = ? AND note LIKE '%s%%'`, NotePrefixRework),
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: pulse: query rework notes: %w", err)
	}
	reworkIDs := make(map[string]bool)
	for reworkRows.Next() {
		var tid string
		if err := reworkRows.Scan(&tid); err != nil {
			_ = reworkRows.Close()
			return nil, fmt.Errorf("quest: pulse: scan rework: %w", err)
		}
		reworkIDs[tid] = true
	}
	if err := reworkRows.Err(); err != nil {
		return nil, fmt.Errorf("quest: pulse: iterate rework: %w", err)
	}
	_ = reworkRows.Close()

	for _, c := range windowCleared {
		if reworkIDs[c.taskID] {
			report.ReworkCount++
		}
	}
	if report.ClearedInWindow > 0 {
		report.ReworkPct = report.ReworkCount * 100 / report.ClearedInWindow
		report.HighRework = report.ReworkPct >= 20
	}

	// --- 3. Spec-update churn: count [spec] notes added after the first
	//     post for each window-cleared quest. First note is the post itself;
	//     subsequent [spec] notes are updates (spec churn). ---
	noteRows, err := db.QueryContext(ctx, //nolint:sqlcheck // NotePrefix* are package constants
		fmt.Sprintf(`SELECT task_id, note FROM task_notes
		 WHERE project_id = ?
		   AND (note LIKE '%s%%' OR note LIKE '%s%%')
		 ORDER BY id ASC`, NotePrefixSpec, NotePrefixSpecReplace),
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: pulse: query notes: %w", err)
	}
	// noteCount[taskID] = number of spec notes (first included for the
	// initial post, extra notes = updates).
	noteCount := make(map[string]int)
	for noteRows.Next() {
		var tid, note string
		if err := noteRows.Scan(&tid, &note); err != nil {
			_ = noteRows.Close()
			return nil, fmt.Errorf("quest: pulse: scan note: %w", err)
		}
		if windowIDs[tid] {
			noteCount[tid]++
		}
	}
	if err := noteRows.Err(); err != nil {
		return nil, fmt.Errorf("quest: pulse: iterate notes: %w", err)
	}
	_ = noteRows.Close()

	// Churn = updates beyond first post = noteCount - 1 (floor 0).
	churns := make([]int, 0, len(windowCleared))
	for _, c := range windowCleared {
		n := noteCount[c.taskID]
		churn := n - 1 // first note is the initial [spec] from Post
		if churn < 0 {
			churn = 0
		}
		churns = append(churns, churn)
	}
	sort.Ints(churns)
	if len(churns) > 0 {
		mid := len(churns) / 2
		if len(churns)%2 == 1 {
			report.ChurnMedian = float64(churns[mid])
		} else {
			report.ChurnMedian = float64(churns[mid-1]+churns[mid]) / 2.0
		}
	}

	// --- 4. Hot files: files appearing in the most quest specs ---
	// Load file specs for all cleared quests (not just window, for
	// stability).
	allClearedIDs := make(map[string]bool, len(allCleared))
	for _, c := range allCleared {
		allClearedIDs[c.taskID] = true
	}

	fileRows, err := db.QueryContext(ctx, //nolint:sqlcheck // NotePrefix* are package constants
		fmt.Sprintf(`SELECT task_id, note FROM task_notes
		 WHERE project_id = ?
		   AND (note LIKE '%sfiles: %%' OR note LIKE '%sfiles: %%')
		 ORDER BY id ASC`, NotePrefixSpec, NotePrefixSpecReplace),
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: pulse: query file notes: %w", err)
	}
	fileQuestCount := make(map[string]map[string]bool) // file → set of quest IDs
	for fileRows.Next() {
		var tid, note string
		if err := fileRows.Scan(&tid, &note); err != nil {
			_ = fileRows.Close()
			return nil, fmt.Errorf("quest: pulse: scan file note: %w", err)
		}
		if !allClearedIDs[tid] {
			continue
		}
		// Parse the files out of the note payload.
		var payload string
		if strings.HasPrefix(note, NotePrefixSpecReplace) {
			payload = note[len(NotePrefixSpecReplace):]
		} else {
			payload = note[len(NotePrefixSpec):]
		}
		// Payload format: "files: a.go, b.go"
		for _, part := range splitSpecParts(payload) {
			k, v, ok := splitKV(part)
			if !ok || k != "files" {
				continue
			}
			for _, f := range splitCommaList(v) {
				if f == "" {
					continue
				}
				if fileQuestCount[f] == nil {
					fileQuestCount[f] = make(map[string]bool)
				}
				fileQuestCount[f][tid] = true
			}
		}
	}
	if err := fileRows.Err(); err != nil {
		return nil, fmt.Errorf("quest: pulse: iterate file notes: %w", err)
	}
	_ = fileRows.Close()

	// Build sorted hot-files list (2+ quests).
	type fileCount struct {
		file  string
		count int
	}
	var fc []fileCount
	for f, qs := range fileQuestCount {
		if len(qs) >= 2 {
			fc = append(fc, fileCount{f, len(qs)})
		}
	}
	sort.Slice(fc, func(i, j int) bool {
		if fc[i].count != fc[j].count {
			return fc[i].count > fc[j].count
		}
		return fc[i].file < fc[j].file
	})
	// Cap at 3.
	limit := 3
	if len(fc) < limit {
		limit = len(fc)
	}
	for _, entry := range fc[:limit] {
		report.HotFiles = append(report.HotFiles, HotFile{entry.file, entry.count})
	}

	// --- 5. No-report count ---
	completedRows, err := db.QueryContext(ctx, //nolint:sqlcheck // NotePrefixCompleted is a package constant
		fmt.Sprintf(`SELECT DISTINCT task_id FROM task_notes
		 WHERE project_id = ? AND note LIKE '%s%%'`, NotePrefixCompleted),
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: pulse: query completed notes: %w", err)
	}
	completedIDs := make(map[string]bool)
	for completedRows.Next() {
		var tid string
		if err := completedRows.Scan(&tid); err != nil {
			_ = completedRows.Close()
			return nil, fmt.Errorf("quest: pulse: scan completed: %w", err)
		}
		completedIDs[tid] = true
	}
	if err := completedRows.Err(); err != nil {
		return nil, fmt.Errorf("quest: pulse: iterate completed: %w", err)
	}
	_ = completedRows.Close()

	for _, c := range windowCleared {
		if !completedIDs[c.taskID] {
			report.NoReport++
		}
	}

	return report, nil
}
