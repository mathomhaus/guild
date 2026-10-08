// QuestCorpus adapts the project-qualified quest search bridge. Migration
// 014 rebuilds historical bridge text from canonical, project-scoped notes
// and invalidates vectors whose old task-only identity could mix projects.
// Completed quests remain eligible for retrieval and embedding.

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

// ActivePredicate includes every quest with searchable spec text, including
// completed quests. Empty bodies have nothing to embed.
func (QuestCorpus) ActivePredicate() string { return "body != ''" }

// SourceTextQuery is shared by reads and transaction-scoped freshness checks.
// The bridge body is the ordered, project-scoped spec text maintained by triggers.
func (QuestCorpus) SourceTextQuery() string {
	return `SELECT body FROM tasks_fts_rows WHERE id = ? AND body != ''`
}

// SourceTextColumn supports batched coverage reads of the canonical text.
func (QuestCorpus) SourceTextColumn() string { return "body" }

// SourceText reads the ordered spec text maintained by project-scoped triggers.
// Missing or empty bridge rows return sql.ErrNoRows.
func (c QuestCorpus) SourceText(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	var text string
	err := db.QueryRowContext(ctx, c.SourceTextQuery(), entityID).Scan(&text)
	return text, err
}

// QuestSourceText assembles the embeddable text for a quest identified
// by its project_id and string task_id. Concatenates spec and spec-replace
// notes in insertion order, separated by newlines. This canonical notes read
// uses exactly the same text convention as the derived bridge body.
//
// Returns ("", sql.ErrNoRows) when the project-qualified task has no spec notes
// (either does not exist or was just created with no notes yet).
func QuestSourceText(ctx context.Context, db *sql.DB, projectID, taskID string) (string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT note FROM task_notes
		 WHERE project_id = ? AND task_id = ? AND (note LIKE '[spec]%' OR note LIKE '[spec-replace]%')
		 ORDER BY id`,
		projectID, taskID,
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
