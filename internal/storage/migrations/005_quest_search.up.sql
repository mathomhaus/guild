-- 005_quest_search.up.sql
--
-- Quest full-text search schema (ADR-003 parallel corpus, LORE-377 / QUEST-224).
--
-- Creates the tables, FTS5 virtual table, sync triggers, vector table,
-- and meta seeds that underpin the quest_search tool.
--
-- Design choices:
--
--   tasks_fts_rows  -- integer-ID bridge table (FTS5 content source).
--                      FTS5 content= requires a TABLE; task_status uses
--                      TEXT PKs ("QUEST-42") so this bridge provides the
--                      INTEGER rowid the algorithms require. It also stores
--                      the searchable body (spec notes concatenated) so
--                      tasks_fts can use content=tasks_fts_rows.
--
--   tasks_fts       -- content-shadow FTS5 index (porter+unicode61 tokenizer,
--                      same as entries_fts after migration 004). Three sync
--                      triggers keep tasks_fts current:
--                        tasks_fts_rows_ai  -- after INSERT on tasks_fts_rows
--                        tasks_fts_rows_au  -- after UPDATE on tasks_fts_rows
--                        tasks_fts_rows_ad  -- after DELETE on tasks_fts_rows
--                      Two more triggers drive tasks_fts_rows:
--                        tasks_fts_status_ai -- INSERT on task_status -> new bridge row
--                        tasks_fts_notes_ai  -- INSERT on task_notes -> update body
--
--   quest_vectors   -- per-quest embedding storage (same shape as lore_vectors,
--                      but entry_id references tasks_fts_rows.id). The embed
--                      algorithms read this shape via QuestCorpus accessors.
--
--   meta seeds      -- 'quest.'-prefixed rows for every MetaField so the
--                      embed algorithms can write without a "no such row" miss.
--
-- Idempotency: schema_migrations prevents re-execution.

-- ---------------------------------------------------------------------------
-- tasks_fts_rows: integer-ID bridge + searchable body accumulator
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS tasks_fts_rows (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id  TEXT    NOT NULL UNIQUE,
  body     TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- tasks_fts: content-shadow FTS5 index over tasks_fts_rows.body
-- ---------------------------------------------------------------------------

CREATE VIRTUAL TABLE IF NOT EXISTS tasks_fts USING fts5(
  body,
  content=tasks_fts_rows, content_rowid=id,
  tokenize = 'porter unicode61 remove_diacritics 1'
);

-- ---------------------------------------------------------------------------
-- FTS sync triggers for tasks_fts_rows changes
-- Mirror the lore entries_fts trigger pattern (see 004_fts5_stopword_tokenizer).
-- ---------------------------------------------------------------------------

CREATE TRIGGER IF NOT EXISTS tasks_fts_rows_ai AFTER INSERT ON tasks_fts_rows BEGIN
  INSERT INTO tasks_fts(rowid, body) VALUES (new.id, new.body);
END;

CREATE TRIGGER IF NOT EXISTS tasks_fts_rows_au AFTER UPDATE OF body ON tasks_fts_rows BEGIN
  INSERT INTO tasks_fts(tasks_fts, rowid, body) VALUES ('delete', old.id, old.body);
  INSERT INTO tasks_fts(rowid, body) VALUES (new.id, new.body);
END;

CREATE TRIGGER IF NOT EXISTS tasks_fts_rows_ad AFTER DELETE ON tasks_fts_rows BEGIN
  INSERT INTO tasks_fts(tasks_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;

-- ---------------------------------------------------------------------------
-- tasks_fts_status_ai: register a bridge row when a quest is first posted.
-- Fires on every INSERT into task_status (quest_post is the only writer
-- that creates new quest IDs).
-- ---------------------------------------------------------------------------

CREATE TRIGGER IF NOT EXISTS tasks_fts_status_ai AFTER INSERT ON task_status BEGIN
  INSERT OR IGNORE INTO tasks_fts_rows (task_id) VALUES (new.task_id);
END;

-- ---------------------------------------------------------------------------
-- tasks_fts_notes_ai: accumulate spec note text into tasks_fts_rows.body.
-- Fires only on [spec] notes (subject / acceptance / files / depends_on lines).
-- Concatenates all current [spec] notes for the quest into the body column;
-- the tasks_fts_rows_au trigger propagates the change to tasks_fts.
-- ---------------------------------------------------------------------------

CREATE TRIGGER IF NOT EXISTS tasks_fts_notes_ai AFTER INSERT ON task_notes
WHEN new.note LIKE '[spec]%'
BEGIN
  UPDATE tasks_fts_rows
  SET body = (
    SELECT group_concat(tn.note, ' ')
    FROM task_notes tn
    WHERE tn.task_id = new.task_id AND tn.note LIKE '[spec]%'
  )
  WHERE task_id = new.task_id;
END;

-- ---------------------------------------------------------------------------
-- quest_vectors: per-quest embedding storage.
-- entry_id references tasks_fts_rows.id (the integer bridge PK).
-- ON DELETE CASCADE so removing a quest cleans its vector row.
-- Shape mirrors lore_vectors (migration 003) so the generic embed
-- algorithms (WriteVector, Backfill, LoadFromDB) need no changes.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS quest_vectors (
  entry_id     INTEGER PRIMARY KEY REFERENCES tasks_fts_rows(id) ON DELETE CASCADE,
  model_id     TEXT    NOT NULL,
  dim          INTEGER NOT NULL,
  vec          BLOB    NOT NULL,
  encoded_at   INTEGER NOT NULL,
  content_hash TEXT    NOT NULL
);

-- ---------------------------------------------------------------------------
-- meta seeds: 'quest.'-prefixed rows for every MetaField.
-- INSERT OR IGNORE so reruns (and partial prior runs) are safe.
-- ---------------------------------------------------------------------------

INSERT OR IGNORE INTO meta (key, value) VALUES
  ('quest.embedder_state',           'disabled'),
  ('quest.embedder_model_id',        ''),
  ('quest.embedder_tokenizer_hash',  ''),
  ('quest.embedder_runtime_version', ''),
  ('quest.embedder_dim',             '0'),
  ('quest.embedder_state_reason',    ''),
  ('quest.vector_epoch',             '0'),
  ('quest.vector_coverage_num',      '0'),
  ('quest.vector_coverage_den',      '0'),
  ('quest.embed_error_count',        '0'),
  ('quest.embed_last_error',         ''),
  ('quest.embed_last_error_at',      ''),
  ('quest.embed_last_ok_at',         '');
