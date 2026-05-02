// Package lore implements the lore (knowledge lifecycle) domain: entries,
// entry links, cross-project dedup, BM25+recency+title-boost appraisal, oath
// loading, and health/dedup operations.
//
// This file carries only shared domain types. Both the write surface
// (inscribe/update/seal/link/reforge) and read surface (appraise/study/oath/...)
// import from here so type definitions stay in sync.
// Behavioural code lives in dedicated files (inscribe.go, appraise.go, ...).
package lore

import "time"

// Kind is the entry classification. Enforced as a string enum at API
// boundaries; the DB stores the raw string per 001_init.up.sql.
type Kind string

const (
	KindIdea        Kind = "idea"
	KindResearch    Kind = "research"
	KindDecision    Kind = "decision"
	KindObservation Kind = "observation"
	KindPrinciple   Kind = "principle"
)

// AllKinds returns the valid kinds in display order.
func AllKinds() []Kind {
	return []Kind{KindIdea, KindResearch, KindDecision, KindObservation, KindPrinciple}
}

// Status tracks an entry's lifecycle state. The full vocabulary is defined
// in 001_init.up.sql. Most entries are "current"; agents rarely author the
// others directly.
type Status string

const (
	StatusCurrent    Status = "current"
	StatusStale      Status = "stale"
	StatusSuperseded Status = "superseded"
	StatusArchived   Status = "archived"
	StatusImported   Status = "imported"
	StatusSeed       Status = "seed"
	StatusExploring  Status = "exploring"
	StatusPromoted   Status = "promoted"
	StatusParked     Status = "parked"
)

// Relation labels an entry_links row. Values are defined in 001_init.up.sql.
type Relation string

const (
	RelationInforms     Relation = "informs"
	RelationSupersedes  Relation = "supersedes"
	RelationContradicts Relation = "contradicts"
)

// Entry is the full row shape from the `entries` table. Nullable SQLite
// columns map to pointer / zero-value semantics documented on each field.
type Entry struct {
	ID             int64
	ProjectID      string
	Topic          string
	Kind           Kind
	Title          string
	Summary        string
	Tags           []string // parsed from comma-separated DB column; empty slice if NULL
	FilePath       string   // "" if NULL
	Source         string   // "" if NULL
	Status         Status
	ValidDays      *int // nil means "never stales"
	NeedsReview    bool
	PromptedBy     string // quest_id that triggered this entry; "" if NULL
	CreatedAt      time.Time
	UpdatedAt      time.Time
	AccessCount    int
	LastAccessedAt *time.Time // nil if never accessed
}

// Link is one row of the entry_links table.
type Link struct {
	FromID    int64
	ToID      int64
	Relation  Relation
	CreatedAt time.Time
}

// EntryID is the human-facing form ("LORE-NNN") used in CLI output and
// between-tool references. Helper for stringification.
func EntryID(id int64) string {
	return formatEntryID(id)
}

// VectorState tracks the per-entry embedding lifecycle in the vector_state
// column on the entries table. The values mirror the ADR-003 state machine.
type VectorState string

const (
	// VectorStatePending means no vector exists yet; the entry is awaiting
	// the backfill pass or a synchronous embed on CLI inscribe.
	VectorStatePending VectorState = "pending"

	// VectorStateIndexed means a current, valid vector row exists in
	// lore_vectors for this entry and the content_hash matches the summary.
	VectorStateIndexed VectorState = "indexed"

	// VectorStateStale means a vector row exists but the summary text has
	// changed since it was encoded (content_hash mismatch). The row will be
	// re-encoded on the next write pass.
	VectorStateStale VectorState = "stale"
)

// MetaKey is a typed key for rows in the meta table. Using a named type
// prevents raw-string typos in callers that read or write meta values.
type MetaKey string

const (
	// MetaEmbedderModelID is the canonical model identity string baked into
	// the binary. Mismatch between this value and a vector row's model_id
	// triggers invalidation.
	MetaEmbedderModelID MetaKey = "embedder_model_id"

	// MetaEmbedderTokenizerHash is the SHA-256 of the tokenizer vocab file
	// embedded in the binary. Updated on model upgrades.
	MetaEmbedderTokenizerHash MetaKey = "embedder_tokenizer_hash" //nolint:gosec // not a credential; this is a meta-table key name

	// MetaEmbedderRuntimeVersion is the ORT library version string used to
	// encode the current vector corpus.
	MetaEmbedderRuntimeVersion MetaKey = "embedder_runtime_version"

	// MetaEmbedderDim is the vector dimension as a decimal string (e.g. "384").
	MetaEmbedderDim MetaKey = "embedder_dim"

	// MetaEmbedderState is "enabled" when the embedder is operational,
	// "disabled" otherwise (Windows, dylib probe failure, explicit opt-out).
	MetaEmbedderState MetaKey = "embedder_state"

	// MetaVectorEpoch is a monotonic counter bumped on every vector write.
	// In-process indexes compare their cached epoch against this value to
	// detect when a reload is needed.
	MetaVectorEpoch MetaKey = "vector_epoch"

	// MetaVectorCoverageNum is the count of entries with a current vector
	// (vector_state = 'indexed'). Updated atomically in Tx2 alongside each
	// vector write.
	MetaVectorCoverageNum MetaKey = "vector_coverage_num"

	// MetaVectorCoverageDen is the count of active entries (not archived,
	// not parked) that are eligible for embedding. Updated by the backfill
	// path and at inscribe/seal time.
	MetaVectorCoverageDen MetaKey = "vector_coverage_den"

	// MetaEmbedErrorCount is a rolling count of Tx2 vector-write failures.
	// Surfaces in guild lore health so repeated failures are visible without
	// log grepping.
	MetaEmbedErrorCount MetaKey = "embed_error_count"
)

// activeEntriesPredicate is the canonical SQL fragment that filters the set
// of entries eligible for embedding. Every site that reads or writes
// vector_coverage_den or vector_coverage_num must use this constant so the
// predicate stays in sync across Inscribe, Seal, Restore, and Backfill.
//
// Matches the seeding WHERE clause in migration 003.
const activeEntriesPredicate = "status NOT IN ('archived', 'parked')"

// sqlBumpCoverageDen is the canonical UPSERT for incrementing
// meta.vector_coverage_den by one. Used by Inscribe and Restore.
const sqlBumpCoverageDen = `INSERT INTO meta (key, value) VALUES ('vector_coverage_den', '1')
ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`

// sqlDecrCoverageDen is the canonical UPDATE for decrementing
// meta.vector_coverage_den by one (floor 0). Used by Seal.
const sqlDecrCoverageDen = `UPDATE meta
SET value = CAST(
  CASE WHEN CAST(value AS INTEGER) > 0
       THEN CAST(value AS INTEGER) - 1
       ELSE 0
  END AS TEXT)
WHERE key = 'vector_coverage_den'`
