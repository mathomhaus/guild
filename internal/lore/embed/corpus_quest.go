// QuestCorpus is the VectorCorpus adapter for the quest tasks schema.
// Maps every port accessor to the schema shipped in migration 005 plus
// the data backfill in migration 006: vectors in quest_vectors, entities
// in tasks_fts_rows (integer-ID bridge, one row per distinct
// task_status.task_id post-006), body column carries concatenated spec
// notes for embedding.
//
// Migration 006 (QUEST-246, LORE-404) is the load-bearing piece: it
// backfills tasks_fts_rows from every distinct task_status.task_id and
// reconstitutes body from each quest's [spec] task_notes. Before 006,
// only quests posted after migration 005 applied were bridged (the
// tasks_fts_status_ai trigger fires on INSERT only and never ran against
// historical rows), so on upgrading installs tasks_fts_rows held a tiny
// fraction of real quests and embeddings silently skipped 96% of the
// corpus. Post-006, COUNT(*) FROM tasks_fts_rows equals COUNT(DISTINCT
// task_id) FROM task_status, and QuestCorpus's effective entity surface
// matches the canonical quest store.
//
// MetaKey returns 'quest.'-prefixed keys so two corpora sharing a
// single meta table never collide on rows (lore owns the unprefixed
// set; quest owns 'quest.*'). This is the prefix-isolation contract
// documented in corpus.go.
//
// No VectorStateColumn: tasks_fts_rows has no vector_state column.
// The embed algorithms skip the per-entity state flip and the state
// predicate in scans when VectorStateColumn() returns the empty string.
//
// ActivePredicate: all bridge rows are eligible for embedding (no
// archive/park lifecycle on quests today). Returns "id IS NOT NULL"
// which the algorithms accept as a pass-through WHERE fragment after
// prefixing the table alias.

package embed

import (
	"context"
	"database/sql"
)

// QuestCorpus adapts the quest tasks_fts_rows schema to VectorCorpus.
type QuestCorpus struct{}

// Compile-time check: QuestCorpus must satisfy VectorCorpus.
var _ VectorCorpus = QuestCorpus{}

// Name is the short tag used in log lines and health reports.
func (QuestCorpus) Name() string { return "quest" }

// VectorTable is the quest_vectors table defined in migration 005.
func (QuestCorpus) VectorTable() string { return "quest_vectors" }

// EntityTable is the integer-ID bridge table defined in migration 005.
// Algorithms treat tasks_fts_rows rows as the embedding subjects.
func (QuestCorpus) EntityTable() string { return "tasks_fts_rows" }

// EntityIDColumn is the tasks_fts_rows PK.
func (QuestCorpus) EntityIDColumn() string { return "id" }

// VectorStateColumn returns "" because tasks_fts_rows has no
// vector_state column. The algorithms skip state-flip UPDATEs and the
// state predicate when the returned string is empty.
func (QuestCorpus) VectorStateColumn() string { return "" }

// ActivePredicate matches all bridge rows. Quest tasks have no
// archive/park lifecycle column today; every row is embed-eligible.
// Returns "id IS NOT NULL" rather than "1=1" because the algorithms
// prepend the table alias ("e.") to this string, producing "e.id IS
// NOT NULL" which is a valid tautological predicate for any row.
func (QuestCorpus) ActivePredicate() string { return "id IS NOT NULL" }

// SourceText reads the searchable text for one entity (bridge row).
// Joins tasks_fts_rows.id -> task_id -> task_notes to assemble all
// [spec] notes for the quest. Returns sql.ErrNoRows when the bridge
// row is gone (deleted mid-backfill) so callers can distinguish that
// from a genuine IO error.
func (QuestCorpus) SourceText(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	var taskID string
	if err := db.QueryRowContext(ctx,
		`SELECT task_id FROM tasks_fts_rows WHERE id = ?`, entityID,
	).Scan(&taskID); err != nil {
		return "", err
	}
	return QuestSourceText(ctx, db, taskID)
}

// QuestSourceText assembles the embeddable text for a quest identified
// by its string task_id. Concatenates all [spec] notes in insertion
// order, separated by newlines. Exported so search_cmd.go can call it
// without importing from an inner package.
//
// Returns ("", sql.ErrNoRows) when the task_id has no [spec] notes
// (either does not exist or was just created with no notes yet).
func QuestSourceText(ctx context.Context, db *sql.DB, taskID string) (string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT note FROM task_notes
		 WHERE task_id = ? AND note LIKE '[spec]%'
		 ORDER BY id`,
		taskID,
	)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var parts []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return "", err
		}
		parts = append(parts, note)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(parts) == 0 {
		return "", sql.ErrNoRows
	}
	return joinLines(parts), nil
}

// joinLines concatenates parts with newline separators.
// Avoids importing strings to keep this file dependency-lean.
func joinLines(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(parts) - 1
	for _, p := range parts {
		n += len(p)
	}
	buf := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, p...)
	}
	return string(buf)
}

// MetaKey maps a MetaField enum to the 'quest.'-prefixed meta key.
// All returned values start with 'quest.' to guarantee isolation from
// LoreCorpus's unprefixed key set (e.g. 'embedder_state' vs
// 'quest.embedder_state'). Stability is a hard contract: changing a
// returned value here IS a migration.
func (QuestCorpus) MetaKey(field MetaField) string {
	switch field {
	case FieldEmbedderState:
		return "quest.embedder_state"
	case FieldEmbedderModelID:
		return "quest.embedder_model_id"
	case FieldEmbedderTokenizerHash:
		return "quest.embedder_tokenizer_hash"
	case FieldEmbedderRuntimeVersion:
		return "quest.embedder_runtime_version"
	case FieldEmbedderDim:
		return "quest.embedder_dim"
	case FieldEmbedderStateReason:
		return "quest.embedder_state_reason"
	case FieldVectorEpoch:
		return "quest.vector_epoch"
	case FieldVectorCoverageNum:
		return "quest.vector_coverage_num"
	case FieldVectorCoverageDen:
		return "quest.vector_coverage_den"
	case FieldEmbedErrorCount:
		return "quest.embed_error_count"
	case FieldEmbedLastError:
		return "quest.embed_last_error"
	case FieldEmbedLastErrorAt:
		return "quest.embed_last_error_at"
	case FieldEmbedLastOKAt:
		return "quest.embed_last_ok_at"
	default:
		return ""
	}
}
