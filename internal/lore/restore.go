package lore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Imported      int // entries newly inscribed
	Skipped       int // entries already present (by title+kind+topic)
	LinksAdded    int // entry_links rows inserted
	SchemaVersion int // the schema_version read from the snapshot
}

// Restore reads snapshotPath (snapshot.json) and re-inscribes the lore
// section into the project identified by projectID.
//
// Version-aware: reads schema_version first and applies version-specific
// deserialization. Unknown future versions emit an informational warning and
// are skipped rather than hard-erroring (forward-compat policy).
//
// Idempotent: entries already present (matched by title+kind+topic within
// the project) are skipped and counted in result.Skipped.
//
// Links are reconstructed using the old-id→new-id map built during import.
func Restore(ctx context.Context, db *sql.DB, projectID, snapshotPath string) (*RestoreResult, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: restore: nil db")
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("lore: restore: projectID required")
	}
	if strings.TrimSpace(snapshotPath) == "" {
		return nil, fmt.Errorf("lore: restore: snapshotPath required")
	}

	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("lore: restore: read %q: %w", snapshotPath, err)
	}

	// Read schema_version first (forward-compat).
	var versionProbe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &versionProbe); err != nil {
		return nil, fmt.Errorf("lore: restore: parse schema_version: %w", err)
	}

	switch versionProbe.SchemaVersion {
	case 1:
		return restoreV1(ctx, db, projectID, data)
	case 0:
		// schema_version absent or zero — legacy format without a version field.
		// Treat as v1 for backward compatibility.
		return restoreV1(ctx, db, projectID, data)
	default:
		// Unknown future version — refuse rather than silently mishandling data.
		// Forward-compat policy: announce at v2, remove support at v3.
		return nil, fmt.Errorf("lore: restore: unsupported schema_version %d (this binary supports v1; upgrade guild to restore v%d snapshots)",
			versionProbe.SchemaVersion, versionProbe.SchemaVersion)
	}
}

// restoreV1 deserialises a schema_version=1 snapshot and re-inscribes
// its lore section. Also handles the legacy format that used "entries"/"links"
// keys instead of "lore"/"links" for backward compat with older snapshots.
func restoreV1(ctx context.Context, db *sql.DB, projectID string, data []byte) (*RestoreResult, error) {
	// v1 format: { "schema_version": 1, "lore": [...], "quest": [...] }
	// Legacy format: { "project_id": ..., "exported_at": ..., "entries": [...], "links": [...] }
	// We support both by checking which key is present.
	var doc struct {
		// v1 fields.
		Lore []snapshotLoreEntry `json:"lore"`
		// Legacy fallback fields.
		Entries []snapshotLoreEntry `json:"entries"`
		// Links are in both formats.
		Links []snapshotLoreLink `json:"links"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("lore: restore v1: unmarshal: %w", err)
	}

	entries := doc.Lore
	if len(entries) == 0 {
		entries = doc.Entries // legacy format fallback
	}
	links := doc.Links

	now := time.Now().UTC().Format(time.RFC3339)
	result := &RestoreResult{SchemaVersion: 1}

	// id_map: old snapshot id → new DB id (for link reconstruction).
	idMap := make(map[int64]int64, len(entries))

	for i := range entries {
		e := &entries[i]
		oldID := e.ID

		// Idempotent check: skip if title+kind+topic already exists in this project.
		var existingID int64
		err := db.QueryRowContext(ctx,
			`SELECT id FROM entries
			  WHERE project_id = ? AND title = ? AND kind = ? AND topic = ?`,
			projectID, e.Title, e.Kind, e.Topic,
		).Scan(&existingID)
		if err == nil {
			// Already exists.
			idMap[oldID] = existingID
			result.Skipped++
			continue
		}
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("lore: restore v1: check existing %d: %w", oldID, err)
		}

		// Resolve valid_days.
		var validDaysArg any
		if e.ValidDays != nil {
			validDaysArg = *e.ValidDays
		}

		tags := strings.TrimSpace(e.Tags)
		filePath := strings.TrimSpace(e.FilePath)
		source := strings.TrimSpace(e.Source)
		promptedBy := strings.TrimSpace(e.PromptedBy)

		status := e.Status
		if status == "" {
			status = string(StatusCurrent)
		}

		createdAt := e.CreatedAt
		if createdAt == "" {
			createdAt = now
		}

		res, err := db.ExecContext(ctx,
			`INSERT INTO entries
			   (project_id, topic, kind, title, summary, tags, file_path, source,
			    status, valid_days, needs_review, prompted_by, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			projectID,
			e.Topic,
			e.Kind,
			e.Title,
			e.Summary,
			nullIfEmpty(tags),
			nullIfEmpty(filePath),
			nullIfEmpty(source),
			status,
			validDaysArg,
			e.NeedsReview,
			nullIfEmpty(promptedBy),
			createdAt,
			now,
		)
		if err != nil {
			return nil, fmt.Errorf("lore: restore v1: insert entry %d: %w", oldID, err)
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("lore: restore v1: last insert id: %w", err)
		}
		idMap[oldID] = newID
		result.Imported++

		// Increment vector_coverage_den for every newly inserted
		// entry whose status counts toward the coverage denominator
		// (everything except archived and parked). Per ADR-003
		// "Mutation semantics", restore is the symmetric counterpart
		// to seal's decrement. Tab-level atomicity here is fine
		// because restore is a single-writer operation; concurrent
		// restore against the same project is not a supported mode.
		if status != string(StatusArchived) && status != string(StatusParked) {
			if _, err := db.ExecContext(ctx, sqlBumpCoverageDen); err != nil {
				return nil, fmt.Errorf("lore: restore v1: bump vector_coverage_den: %w", err)
			}
		}
	}

	// Reconstruct links using the id_map.
	for _, l := range links {
		fromID, fromOK := idMap[l.FromID]
		toID, toOK := idMap[l.ToID]
		if !fromOK || !toOK {
			continue // id not in map → skip
		}
		relation := l.Relation
		if relation == "" {
			relation = string(RelationInforms)
		}
		_, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO entry_links (from_id, to_id, relation) VALUES (?, ?, ?)`,
			fromID, toID, relation,
		)
		if err != nil {
			return nil, fmt.Errorf("lore: restore v1: insert link %d→%d: %w", fromID, toID, err)
		}
		result.LinksAdded++
	}

	return result, nil
}
