-- 003_embeddings_schema.up.sql: Phase 1.2 of ADR-003. lore_vectors table,
-- vector_state column on entries, meta key-value store, and seeded embedder
-- rows.
--
-- Schema-only substrate. No Go code reads or writes these tables yet; that
-- is the job of QUEST-209 (index), QUEST-210 (init/backfill), QUEST-211
-- (health), and QUEST-212 (MCP/CLI wiring).
--
-- Idempotency: the migration runner (migrate.go) records each version in
-- schema_migrations and never re-applies a version. The IF NOT EXISTS guards
-- on CREATE TABLE/INDEX and INSERT OR IGNORE on meta seeds provide
-- defense-in-depth for anyone who replays the SQL directly.
--
-- ALTER TABLE ADD COLUMN does not support IF NOT EXISTS in SQLite. Since the
-- runner applies each version exactly once, this is safe. On the rare case of
-- direct SQL replay, the "duplicate column name" error is the caller's problem.

-- ---------------------------------------------------------------------------
-- meta: global key-value store for embedder identity and coverage tracking.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- ---------------------------------------------------------------------------
-- lore_vectors: one row per embedded lore entry.
-- FK ON DELETE CASCADE so hard-deleting an entry removes its vector row.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS lore_vectors (
  entry_id     INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
  model_id     TEXT    NOT NULL,
  dim          INTEGER NOT NULL,
  vec          BLOB    NOT NULL,
  encoded_at   INTEGER NOT NULL,
  content_hash TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_lore_vectors_model ON lore_vectors(model_id);

-- ---------------------------------------------------------------------------
-- Add vector_state to entries. Default 'pending' marks every existing row
-- for backfill automatically. NOT NULL + DEFAULT covers both new inserts and
-- the existing rows that receive the default value upon ALTER TABLE.
-- ---------------------------------------------------------------------------

ALTER TABLE entries ADD COLUMN vector_state TEXT NOT NULL DEFAULT 'pending';

-- ---------------------------------------------------------------------------
-- Seed the eight embedder meta rows.
-- INSERT OR IGNORE: rows already present (e.g. on manual SQL replay) are
-- left unchanged so operator edits survive.
--
-- vector_coverage_den is initialized to the count of active entries at
-- migration time (status not 'archived' and not 'parked') rather than zero,
-- so coverage ratio is meaningful immediately. The backfill/init path
-- recomputes the denominator; this seed just avoids a spurious 0/0 state.
-- ---------------------------------------------------------------------------

INSERT OR IGNORE INTO meta (key, value) VALUES
  ('embedder_model_id',        'bge-small-en-v1.5-int8-cls'),
  ('embedder_tokenizer_hash',  ''),
  ('embedder_runtime_version', 'onnxruntime-1.23.x'),
  ('embedder_dim',             '384'),
  ('embedder_state',           'disabled'),
  ('vector_epoch',             '0'),
  ('vector_coverage_num',      '0'),
  ('embed_error_count',        '0');

INSERT OR IGNORE INTO meta (key, value)
  SELECT 'vector_coverage_den',
         CAST(COUNT(*) AS TEXT)
  FROM   entries
  WHERE  status NOT IN ('archived', 'parked');
