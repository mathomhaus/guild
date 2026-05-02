// Package embed's VectorCorpus port defines the boundary between the
// storage-agnostic vector algorithms (Index, Backfill, WriteVector,
// ReadHealthReport) and any concrete corpus (lore today; quest,
// session, mathom, code-embeddings tomorrow). The port is a set of
// small accessors that describe a corpus in terms the algorithms need:
// the SQL shape (tables, columns, predicate), the source-text lookup
// for embedding, and the meta-key resolver for per-corpus counters.
//
// Hexagonal position:
//
//	domain services: internal/lore/embed/{index,backfill,hot,health}.go
//	port:            VectorCorpus (this file)
//	adapters:        internal/lore/embed/corpus_lore.go (LoreCorpus),
//	                 future corpus_quest.go, corpus_session.go, ...
//
// Interface Segregation: the facade is composed of three narrow
// sub-interfaces so an adapter needing only some capabilities can
// depend on the smaller surface. CorpusSchema changes when the SQL
// shape evolves; CorpusSources changes when text-assembly evolves;
// CorpusMeta changes when meta-key semantics evolve. Each has exactly
// one reason to change (Single Responsibility).
//
// Orthogonality to the Embedder axis: this port sits on the storage
// dimension. The embedder-implementation dimension (BGE today,
// mathomhaus/ortpipe in ADR-003 Phase 2, future model swaps) evolves
// independently. An Index, a Backfill, a WriteVector, or a
// ReadHealthReport takes a VectorCorpus AND an Embedder; swapping one
// does not constrain the other. This decoupling is a design invariant,
// not an accident, and must be preserved when the Phase 2 swap lands.
//
// Backward compatibility: LoreCorpus.MetaKey returns UNPREFIXED keys
// matching the schema shipped in migrations 001 through 004
// ('embedder_state', 'vector_epoch', etc.). No DB migration is needed
// to land this port. Future corpora (QuestCorpus and friends) will
// return PREFIXED keys ('quest.vector_epoch', etc.) and own their own
// migrations; the prefixing guarantees two corpora sharing a single DB
// never collide on meta rows.

package embed

import (
	"context"
	"database/sql"
)

// MetaField enumerates every meta row the vector algorithms read or
// write. A MetaKey adapter maps these enum values to concrete string
// keys; the algorithms never mention the strings directly. Adding a
// new meta row is a two-line change (add a constant here, map it in
// every adapter) rather than a grep across packages.
type MetaField int

// MetaField constants cover the full meta surface the algorithms use.
// Ordering is stable but not load-bearing: callers must always
// reference the symbolic name, never the integer value.
const (
	// FieldEmbedderState is the 'enabled' | 'disabled' switch written
	// by PrepareAndProbe / WriteMeta and read by every wire-up site.
	FieldEmbedderState MetaField = iota

	// FieldEmbedderModelID is the canonical model_id the binary bakes
	// in; mismatch between this and a vector row's model_id invalidates
	// the row.
	FieldEmbedderModelID

	// FieldEmbedderTokenizerHash is the SHA-256 of the tokenizer vocab
	// file the binary embeds.
	FieldEmbedderTokenizerHash

	// FieldEmbedderRuntimeVersion is the ORT runtime version string
	// the binary was built against.
	FieldEmbedderRuntimeVersion

	// FieldEmbedderDim is the vector dimension as a decimal string.
	FieldEmbedderDim

	// FieldEmbedderStateReason is the machine-parseable reason tag for
	// a disabled embedder (probe_mismatch, extract_failed, ...).
	FieldEmbedderStateReason

	// FieldVectorEpoch is the monotonic counter the in-memory Index
	// consults via CheckAndReload.
	FieldVectorEpoch

	// FieldVectorCoverageNum is the count of 'indexed' rows (vectors
	// present and current).
	FieldVectorCoverageNum

	// FieldVectorCoverageDen is the count of active rows eligible for
	// embedding (not archived, not parked, etc.).
	FieldVectorCoverageDen

	// FieldEmbedErrorCount is the rolling count of Tx2 vector-write
	// failures.
	FieldEmbedErrorCount

	// FieldEmbedLastError is the last error message string.
	FieldEmbedLastError

	// FieldEmbedLastErrorAt is the RFC3339 timestamp of the last
	// error.
	FieldEmbedLastErrorAt

	// FieldEmbedLastOKAt is the RFC3339 timestamp of the last
	// successful encode.
	FieldEmbedLastOKAt
)

// CorpusSchema describes the SQL shape of a corpus. Algorithms compose
// table and column names from these accessors so a new corpus becomes
// an adapter, not a fork. Every accessor returns a stable string; the
// values originate in compile-time adapter code and are safe for
// fmt.Sprintf templating (no user input).
type CorpusSchema interface {
	// Name is the short corpus identifier ("lore", "quest", ...) used
	// in log lines and health reports so operators can tell which
	// corpus produced what.
	Name() string

	// VectorTable is the SQL table that stores the vector blobs plus
	// their model_id and content_hash. Must share the ADR-003 shape:
	// (entity_id PK, model_id TEXT, dim INT, vec BLOB, encoded_at INT,
	// content_hash TEXT).
	VectorTable() string

	// EntityTable is the SQL table whose rows are the embedding
	// subjects. For lore that is 'entries'; for a future quest corpus
	// it is 'quests'.
	EntityTable() string

	// EntityIDColumn is the primary-key column of EntityTable. Always
	// 'id' in the current schema; left as an accessor to permit future
	// corpora that use a composite or renamed key without editing the
	// algorithms.
	EntityIDColumn() string

	// VectorStateColumn is the column on EntityTable that records the
	// per-entity embedding lifecycle ('pending' | 'indexed' | 'stale').
	// Corpora that do not track state return the empty string; the
	// algorithms then skip the state-flip UPDATE and the
	// vector_state predicate in scans.
	VectorStateColumn() string

	// ActivePredicate is the SQL fragment (without a leading WHERE /
	// AND) that filters EntityTable to the subset eligible for
	// embedding. For lore it is "status NOT IN ('archived', 'parked')".
	// Must never reference a column outside EntityTable.
	ActivePredicate() string
}

// CorpusSources describes how to assemble the text the embedder
// encodes. Split from CorpusSchema because text-assembly can evolve
// independently (a future corpus might concatenate title + body or
// strip code fences before embedding) without the SQL shape changing.
type CorpusSources interface {
	// SourceText reads the text to embed for a single entity by id.
	// Returns ("", sql.ErrNoRows) for a missing row so callers can
	// distinguish "deleted mid-backfill" from "genuine read error".
	SourceText(ctx context.Context, db *sql.DB, entityID int64) (string, error)
}

// CorpusMeta maps the MetaField enum to concrete meta-table keys.
// Split out because two corpora can share a single meta table; the
// prefixing scheme (unprefixed for lore, 'quest.' for quest, ...) is
// exclusively this adapter's concern. The algorithms never see the
// string keys.
type CorpusMeta interface {
	// MetaKey returns the meta-table string key for a given field.
	// Must be stable across binary versions for a given corpus:
	// changing the returned key is a schema migration, not a refactor.
	MetaKey(field MetaField) string
}

// VectorCorpus is the composed facade the algorithms consume. An
// adapter implements all three sub-interfaces; callers pass the
// composed type into Index, Backfill, WriteVector, ReadHealthReport.
// The facade is intentionally small; growing it requires a conscious
// decision about which sub-interface a new capability belongs to.
type VectorCorpus interface {
	CorpusSchema
	CorpusSources
	CorpusMeta
}
