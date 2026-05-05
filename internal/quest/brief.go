package quest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// briefTaskID is the synthetic task_id used for project-level notes.
const briefTaskID = "__PROJECT__"

// briefPrefix is the note prefix for brief entries.
const briefPrefix = "[brief] "

// briefStaleThreshold is the window within which a quest_brief is considered
// "fresh". If the most recent brief for a project is older than this, or
// absent entirely, quest_clear emits an advisory hint.
const briefStaleThreshold = 24 * time.Hour

// Brief persists a session-end handoff note for projectID. Bounties
// surfaces the most recent brief at the start of the next session.
//
// The note is stored in task_notes with task_id="__PROJECT__" and
// note="[brief] <text>".
//
// agent identifies who wrote the brief; if empty it defaults via
// journalAgent.
func Brief(ctx context.Context, db *sql.DB, projectID, text, agent string) error {
	if db == nil {
		return fmt.Errorf("quest: brief: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: brief: empty project_id")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("quest: brief: empty text")
	}
	agent = journalAgent(agent)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	_, err := db.ExecContext(ctx,
		`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		projectID, briefTaskID, agent, briefPrefix+text, now,
	)
	if err != nil {
		return fmt.Errorf("quest: brief: insert: %w", err)
	}
	return nil
}

// LastBriefAt returns the timestamp of the most recent quest_brief for
// projectID. Returns zero time and nil error if no brief exists — an empty
// history is valid, not an error. Used by quest_clear to decide whether to
// emit the stale-brief advisory hint.
func LastBriefAt(ctx context.Context, db *sql.DB, projectID string) (time.Time, error) {
	if db == nil {
		return time.Time{}, fmt.Errorf("quest: last brief at: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return time.Time{}, fmt.Errorf("quest: last brief at: empty project_id")
	}

	var createdAt string
	qerr := db.QueryRowContext(ctx,
		`SELECT created_at FROM task_notes
		 WHERE project_id = ? AND task_id = ? AND note LIKE '[brief]%'
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		projectID, briefTaskID,
	).Scan(&createdAt)
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("quest: last brief at: query: %w", qerr)
	}
	return parseNoteTime(createdAt), nil
}

// LastBrief returns the most recent brief for projectID, or nil, "",
// time.Time{} if none exists. Used by Bounties to surface the last
// session's handoff.
func LastBrief(ctx context.Context, db *sql.DB, projectID string) (agentID, text string, at time.Time, err error) {
	if db == nil {
		return "", "", time.Time{}, fmt.Errorf("quest: last brief: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return "", "", time.Time{}, fmt.Errorf("quest: last brief: empty project_id")
	}

	var agent, note, createdAt string
	qerr := db.QueryRowContext(ctx,
		`SELECT agent_id, note, created_at FROM task_notes
		 WHERE project_id = ? AND task_id = ? AND note LIKE '[brief]%'
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		projectID, briefTaskID,
	).Scan(&agent, &note, &createdAt)
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return "", "", time.Time{}, nil
		}
		return "", "", time.Time{}, fmt.Errorf("quest: last brief: query: %w", qerr)
	}

	// Strip the "[brief] " prefix to return the plain text.
	rawText := note
	if strings.HasPrefix(rawText, briefPrefix) {
		rawText = rawText[len(briefPrefix):]
	}

	t := parseNoteTime(createdAt)
	return agent, rawText, t, nil
}
