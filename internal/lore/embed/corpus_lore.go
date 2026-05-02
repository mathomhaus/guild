// LoreCorpus is the VectorCorpus adapter for the lore entries table.
// Maps every port accessor to the schema shipped in migrations 001
// through 004: vectors in lore_vectors, entities in entries,
// vector_state column on entries, activePredicate matching the
// seed-time WHERE clause in migration 003.
//
// MetaKey returns UNPREFIXED keys for backward compatibility. The lore
// corpus has been writing 'embedder_state', 'vector_epoch', etc. since
// Phase 1 shipped; adding a prefix here would require a migration and
// break every `guild lore health` invocation on an existing DB. Future
// corpora (QuestCorpus, SessionCorpus, ...) MUST use a prefix of their
// own ('quest.vector_epoch', 'session.embedder_state') so two corpora
// sharing a single DB never collide on meta rows.
//
// This is a pure value type with no fields. The adapter is
// indistinguishable from its zero value; callers construct it as
// LoreCorpus{} at every wire-up site.

package embed

import (
	"context"
	"database/sql"
)

// LoreCorpus adapts the lore entries schema to VectorCorpus.
type LoreCorpus struct{}

// Compile-time check: LoreCorpus must satisfy VectorCorpus. Any gap
// surfaces as a build error rather than a runtime type-assertion
// failure at the first wire-up site.
var _ VectorCorpus = LoreCorpus{}

// Name is the short tag used in log lines and health reports.
func (LoreCorpus) Name() string { return "lore" }

// VectorTable is the canonical lore_vectors table defined in
// migration 003.
func (LoreCorpus) VectorTable() string { return "lore_vectors" }

// EntityTable is the lore entries table defined in migration 001.
func (LoreCorpus) EntityTable() string { return "entries" }

// EntityIDColumn is the entries PK.
func (LoreCorpus) EntityIDColumn() string { return "id" }

// VectorStateColumn is the per-entry embedding lifecycle column added
// in migration 003 (values: 'pending' | 'indexed' | 'stale').
func (LoreCorpus) VectorStateColumn() string { return "vector_state" }

// ActivePredicate matches the canonical embed-eligibility predicate
// shared by Backfill, ReconcileDen, Invalidate, and the parallel copy
// in internal/lore/types.go.
func (LoreCorpus) ActivePredicate() string { return activeEntriesPredicate }

// SourceText reads entries.summary for one entity. Returns
// sql.ErrNoRows when the entity is gone (deleted mid-backfill) so
// callers can distinguish that from an IO error.
func (LoreCorpus) SourceText(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	var summary string
	err := db.QueryRowContext(ctx,
		`SELECT summary FROM entries WHERE id = ?`,
		entityID,
	).Scan(&summary)
	if err != nil {
		return "", err
	}
	return summary, nil
}

// MetaKey maps a MetaField enum to the unprefixed meta key the lore
// schema has shipped with since Phase 1. Stability is a hard contract:
// changing a returned value here IS a migration.
func (LoreCorpus) MetaKey(field MetaField) string {
	switch field {
	case FieldEmbedderState:
		return "embedder_state"
	case FieldEmbedderModelID:
		return "embedder_model_id"
	case FieldEmbedderTokenizerHash:
		return "embedder_tokenizer_hash"
	case FieldEmbedderRuntimeVersion:
		return "embedder_runtime_version"
	case FieldEmbedderDim:
		return "embedder_dim"
	case FieldEmbedderStateReason:
		return "embedder_state_reason"
	case FieldVectorEpoch:
		return "vector_epoch"
	case FieldVectorCoverageNum:
		return "vector_coverage_num"
	case FieldVectorCoverageDen:
		return "vector_coverage_den"
	case FieldEmbedErrorCount:
		return "embed_error_count"
	case FieldEmbedLastError:
		return "embed_last_error"
	case FieldEmbedLastErrorAt:
		return "embed_last_error_at"
	case FieldEmbedLastOKAt:
		return "embed_last_ok_at"
	default:
		// Unknown enum value: return empty string so callers can
		// detect and fail loudly rather than writing to a silently
		// mistyped key. An UNREACHABLE by construction; adding a new
		// MetaField without a case here is a test failure on any
		// parameterized suite that iterates the enum.
		return ""
	}
}
