package lore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SnapshotVersion is the schema_version value embedded in snapshot.json.
// v1 is the stable shape; restore accepts v0 (legacy, no version field) as
// an alias for v1.
const SnapshotVersion = 1

// snapshotLoreEntry is the JSON shape for one entry in the lore section of
// snapshot.json. Field names are stable across versions so restore can
// round-trip archives created by any compatible binary.
type snapshotLoreEntry struct {
	ID          int64  `json:"id"`
	ProjectID   string `json:"project_id"`
	Topic       string `json:"topic"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Body        string `json:"body,omitempty"`
	Tags        string `json:"tags,omitempty"`
	FilePath    string `json:"file_path,omitempty"`
	Source      string `json:"source,omitempty"`
	Status      string `json:"status"`
	ValidDays   *int   `json:"valid_days,omitempty"`
	NeedsReview int    `json:"needs_review"`
	PromptedBy  string `json:"prompted_by,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	AccessCount int    `json:"access_count"`
}

// snapshotLoreLink is the JSON shape for one entry_links row.
type snapshotLoreLink struct {
	FromID   int64  `json:"from_id"`
	ToID     int64  `json:"to_id"`
	Relation string `json:"relation"`
}

// snapshot is the top-level snapshot.json document.
// The lore section is what Archive writes; the quest section is preserved
// read-modify-write so a quest archive run doesn't clobber ours.
// Links carries the provenance edges (entry_links) so restore can rebuild
// the full graph, not just the nodes.
type snapshot struct {
	SchemaVersion int                 `json:"schema_version"`
	SnapshotAt    string              `json:"snapshot_at"`
	ProducedBy    string              `json:"produced_by"`
	Lore          []snapshotLoreEntry `json:"lore"`
	Links         []snapshotLoreLink  `json:"links,omitempty"`
	Quest         json.RawMessage     `json:"quest,omitempty"`
}

// GuildVersion is the produced_by string embedded in snapshot.json.
// It identifies the producer version for diagnostics.
const GuildVersion = "guild 0.1.0"

// Archive writes <snapshotPath> (typically <repo>/.guild/snapshot.json)
// with schema_version=1, produced_by, snapshot_at, lore:[...].
//
// Read-modify-write: if snapshotPath already exists, Archive reads the
// existing quest section and preserves it unchanged in the output — so
// a quest archive run doesn't get nuked by a subsequent lore archive
// and vice-versa.
//
// Atomic write: the file is written to <path>.tmp then renamed over the
// final path. Crash-safe.
func Archive(ctx context.Context, db *sql.DB, projectID, snapshotPath string) error {
	if db == nil {
		return fmt.Errorf("lore: archive: nil db")
	}
	if strings.TrimSpace(projectID) == "" {
		return fmt.Errorf("lore: archive: projectID required")
	}
	if strings.TrimSpace(snapshotPath) == "" {
		return fmt.Errorf("lore: archive: snapshotPath required")
	}

	// Read existing snapshot to preserve the quest section.
	var existingQuest json.RawMessage
	if data, err := os.ReadFile(snapshotPath); err == nil {
		// File exists — extract the quest section.
		var existing snapshot
		if jsonErr := json.Unmarshal(data, &existing); jsonErr == nil {
			existingQuest = existing.Quest
		}
	}
	// err == nil → file existed; err != nil (e.g. ErrNotExist) → new file; both fine.

	// Ensure the directory exists.
	dir := filepath.Dir(snapshotPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("lore: archive: mkdir %q: %w", dir, err)
	}

	// Fetch all entries for this project.
	entryRows, err := db.QueryContext(ctx,
		`SELECT id, project_id, topic, kind, title, summary, body,
		        COALESCE(tags,''), COALESCE(file_path,''), COALESCE(source,''),
		        status, valid_days, needs_review,
		        COALESCE(prompted_by,''), created_at, updated_at, access_count
		   FROM entries
		  WHERE project_id = ?
		  ORDER BY id`,
		projectID,
	)
	if err != nil {
		return fmt.Errorf("lore: archive: query entries: %w", err)
	}
	defer func() { _ = entryRows.Close() }()

	var loreEntries []snapshotLoreEntry
	for entryRows.Next() {
		var e snapshotLoreEntry
		var validDays sql.NullInt64
		if err := entryRows.Scan(
			&e.ID, &e.ProjectID, &e.Topic, &e.Kind, &e.Title, &e.Summary, &e.Body,
			&e.Tags, &e.FilePath, &e.Source,
			&e.Status, &validDays, &e.NeedsReview,
			&e.PromptedBy, &e.CreatedAt, &e.UpdatedAt, &e.AccessCount,
		); err != nil {
			return fmt.Errorf("lore: archive: scan entry: %w", err)
		}
		if validDays.Valid {
			v := int(validDays.Int64)
			e.ValidDays = &v
		}
		loreEntries = append(loreEntries, e)
	}
	if err := entryRows.Err(); err != nil {
		return fmt.Errorf("lore: archive: iterate entries: %w", err)
	}

	// If no entries found, write an empty lore array (not null).
	if loreEntries == nil {
		loreEntries = []snapshotLoreEntry{}
	}

	// Build the set of entry IDs present in this archive so we can filter
	// edges. Cross-project links whose far endpoint falls outside this snapshot
	// would be dangling references on restore — omit them.
	entryIDSet := make(map[int64]struct{}, len(loreEntries))
	for i := range loreEntries {
		entryIDSet[loreEntries[i].ID] = struct{}{}
	}

	// Fetch provenance edges where both endpoints are in the archived set.
	linkRows, err := db.QueryContext(ctx,
		`SELECT from_id, to_id, relation
		   FROM entry_links
		  WHERE from_id IN (SELECT id FROM entries WHERE project_id = ?)
		     OR to_id   IN (SELECT id FROM entries WHERE project_id = ?)`,
		projectID, projectID,
	)
	if err != nil {
		return fmt.Errorf("lore: archive: query links: %w", err)
	}
	defer func() { _ = linkRows.Close() }()

	var loreLinks []snapshotLoreLink
	for linkRows.Next() {
		var l snapshotLoreLink
		if err := linkRows.Scan(&l.FromID, &l.ToID, &l.Relation); err != nil {
			return fmt.Errorf("lore: archive: scan link: %w", err)
		}
		// Only emit the edge when both endpoints are in the snapshot.
		if _, fromOK := entryIDSet[l.FromID]; !fromOK {
			continue
		}
		if _, toOK := entryIDSet[l.ToID]; !toOK {
			continue
		}
		loreLinks = append(loreLinks, l)
	}
	if err := linkRows.Err(); err != nil {
		return fmt.Errorf("lore: archive: iterate links: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	snap := snapshot{
		SchemaVersion: SnapshotVersion,
		SnapshotAt:    now,
		ProducedBy:    GuildVersion,
		Lore:          loreEntries,
		Links:         loreLinks,
		Quest:         existingQuest,
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("lore: archive: marshal json: %w", err)
	}
	data = append(data, '\n')

	// Atomic write: tmp file → rename.
	tmpPath := snapshotPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("lore: archive: write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, snapshotPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("lore: archive: rename to final: %w", err)
	}

	return nil
}
