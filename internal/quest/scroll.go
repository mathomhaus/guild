package quest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NoteEntry is one row from task_notes for a quest.
type NoteEntry struct {
	AgentID   string
	Note      string
	CreatedAt time.Time
}

// EventEntry is one row from task_events for a quest.
type EventEntry struct {
	Event     string
	AgentID   string
	Data      string
	CreatedAt time.Time
}

// ScrollResult is the full history view of a quest: resolved spec,
// current status, all notes, and all events in chronological order.
type ScrollResult struct {
	Quest  *Quest
	Notes  []NoteEntry
	Events []EventEntry
}

// Scroll returns the full history of questID: current status, all notes,
// and all task_events, ordered chronologically.
//
// Returns ErrNotFound when questID has no row in task_status.
func Scroll(ctx context.Context, db *sql.DB, projectID, questID string) (*ScrollResult, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: scroll: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: scroll: empty project_id")
	}
	questID = strings.ToUpper(strings.TrimSpace(questID))
	if questID == "" {
		return nil, fmt.Errorf("quest: scroll: empty quest_id")
	}

	q, err := Load(ctx, db, projectID, questID)
	if err != nil {
		return nil, err
	}

	notes, err := loadNotes(ctx, db, projectID, questID)
	if err != nil {
		return nil, err
	}

	events, err := loadEvents(ctx, db, projectID, questID)
	if err != nil {
		return nil, err
	}

	return &ScrollResult{
		Quest:  q,
		Notes:  notes,
		Events: events,
	}, nil
}

// loadNotes returns all task_notes rows for questID, oldest first.
func loadNotes(ctx context.Context, db *sql.DB, projectID, questID string) ([]NoteEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, agent_id, note, created_at
		 FROM task_notes
		 WHERE project_id = ? AND task_id = ?
		 ORDER BY created_at ASC, id ASC`,
		projectID, questID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: scroll: load notes %s: %w", questID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NoteEntry
	for rows.Next() {
		var id int64
		var agentID, note, createdAt string
		if err := rows.Scan(&id, &agentID, &note, &createdAt); err != nil {
			return nil, fmt.Errorf("quest: scroll: scan note: %w", err)
		}
		t := parseNoteTime(createdAt)
		out = append(out, NoteEntry{AgentID: agentID, Note: note, CreatedAt: t})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: scroll: iterate notes %s: %w", questID, err)
	}
	return out, nil
}

// loadEvents returns all task_events rows for questID, oldest first.
func loadEvents(ctx context.Context, db *sql.DB, projectID, questID string) ([]EventEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, event, COALESCE(agent_id,''), COALESCE(data,''), created_at
		 FROM task_events
		 WHERE project_id = ? AND task_id = ?
		 ORDER BY created_at ASC, id ASC`,
		projectID, questID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: scroll: load events %s: %w", questID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventEntry
	for rows.Next() {
		var id int64
		var event, agentID, data, createdAt string
		if err := rows.Scan(&id, &event, &agentID, &data, &createdAt); err != nil {
			return nil, fmt.Errorf("quest: scroll: scan event: %w", err)
		}
		t := parseNoteTime(createdAt)
		out = append(out, EventEntry{Event: event, AgentID: agentID, Data: data, CreatedAt: t})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: scroll: iterate events %s: %w", questID, err)
	}
	return out, nil
}

// parseNoteTime parses the various timestamp formats used in task_notes /
// task_events (RFC3339Nano, RFC3339, or SQLite datetime format).
// Returns the zero time when the string is unparseable.
func parseNoteTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
