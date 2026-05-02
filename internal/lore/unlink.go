package lore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// UnlinkResult carries the outcome of an UnlinkEntries call.
type UnlinkResult struct {
	// Removed is true when a row was actually deleted.
	Removed bool
	// Note is a human-readable status phrase:
	//   "edge removed" when Removed is true
	//   "no matching edge" when Removed is false (idempotent no-op)
	Note string
}

// UnlinkEntries removes the entry_links row keyed by (fromID, toID, rel).
// When rel is empty it defaults to RelationInforms, mirroring LinkEntries.
//
// Idempotent: if no matching row exists the function returns a non-error
// UnlinkResult with Note="no matching edge" so retries are safe.
//
// Cross-project edges are supported because entry_links has no project_id
// column. The lookup is by the numeric IDs alone, matching LinkEntries.
//
// Validation mirrors LinkEntries: rel must be a known Relation value;
// fromID and toID must both resolve to existing entries (cross-project).
// Self-unlink (fromID==toID) is rejected for the same reason as self-link.
func UnlinkEntries(ctx context.Context, db *sql.DB, fromID, toID int64, rel Relation) (UnlinkResult, error) {
	if db == nil {
		return UnlinkResult{}, fmt.Errorf("lore: unlink: nil db")
	}
	rel = Relation(strings.TrimSpace(string(rel)))
	if rel == "" {
		rel = RelationInforms
	}
	if !isValidRelation(rel) {
		return UnlinkResult{}, fmt.Errorf("%w: %q", ErrInvalidRelation, string(rel))
	}
	if fromID == toID {
		return UnlinkResult{}, fmt.Errorf("%w: %s", ErrSelfLink, formatEntryID(fromID))
	}

	// Verify both entries exist before touching entry_links. This keeps
	// the error semantics consistent with LinkEntries and catches typos
	// before mutating.
	if err := requireEntryExists(ctx, db, fromID); err != nil {
		return UnlinkResult{}, err
	}
	if err := requireEntryExists(ctx, db, toID); err != nil {
		return UnlinkResult{}, err
	}

	res, err := db.ExecContext(ctx,
		`DELETE FROM entry_links
		 WHERE from_id = ? AND to_id = ? AND relation = ?`,
		fromID, toID, string(rel),
	)
	if err != nil {
		return UnlinkResult{}, fmt.Errorf("lore: unlink: delete: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return UnlinkResult{}, fmt.Errorf("lore: unlink: rows affected: %w", err)
	}

	if rows == 0 {
		return UnlinkResult{Removed: false, Note: "no matching edge"}, nil
	}
	return UnlinkResult{Removed: true, Note: "edge removed"}, nil
}
