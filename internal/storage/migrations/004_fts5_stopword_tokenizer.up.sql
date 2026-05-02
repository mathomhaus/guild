-- 004_fts5_stopword_tokenizer.up.sql
--
-- Phase 0 of ADR-003: upgrade the lore FTS5 index tokenizer from the
-- default unicode61 to porter+unicode61 with diacritics removed. Porter
-- stemming collapses morphological variants (retry/retried/retrying) at
-- index time so that exact-technical and morphological queries both improve.
--
-- Query-side stopword filtering lives in internal/lore/appraise.go
-- (ftsQuery strips BM25Stopwords before building the MATCH expression).
-- Together these two changes measured +12.4pp Recall@5 on natural-language
-- queries in the 2026-04-23 spike (315-entry corpus, 42-query eval set).
--
-- Migration strategy:
--   1. Drop the FTS5 virtual table and its sync triggers (safe because
--      entries_fts is a contentless-shadow index: all content lives in
--      the `entries` table which this migration never touches).
--   2. Recreate entries_fts with the porter tokenizer.
--   3. Rebuild the index from entries (INSERT INTO entries_fts ... rebuild).
--   4. Recreate the three sync triggers.
--
-- Idempotency: schema_migrations prevents re-execution. The DROP IF EXISTS
-- guards handle the edge case where a partial prior run left the table in
-- an inconsistent state.

DROP TRIGGER IF EXISTS entries_ai;
DROP TRIGGER IF EXISTS entries_ad;
DROP TRIGGER IF EXISTS entries_au;
DROP TABLE IF EXISTS entries_fts;

CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
  title, summary, tags,
  content=entries, content_rowid=id,
  tokenize = 'porter unicode61 remove_diacritics 1'
);

INSERT INTO entries_fts(entries_fts) VALUES('rebuild');

CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
  INSERT INTO entries_fts(rowid, title, summary, tags)
  VALUES (new.id, new.title, new.summary, COALESCE(new.tags, ''));
END;

CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
  INSERT INTO entries_fts(entries_fts, rowid, title, summary, tags)
  VALUES ('delete', old.id, old.title, old.summary, COALESCE(old.tags, ''));
END;

CREATE TRIGGER IF NOT EXISTS entries_au AFTER UPDATE OF title, summary, tags ON entries BEGIN
  INSERT INTO entries_fts(entries_fts, rowid, title, summary, tags)
  VALUES ('delete', old.id, old.title, old.summary, COALESCE(old.tags, ''));
  INSERT INTO entries_fts(rowid, title, summary, tags)
  VALUES (new.id, new.title, new.summary, COALESCE(new.tags, ''));
END;
