-- 006_backfill_tasks_fts_rows.up.sql
--
-- Backfill historical task_status rows into tasks_fts_rows so QuestCorpus
-- (the embedding adapter) and quest_search (BM25 + RRF) see every quest,
-- not just the ones posted after migration 005 applied (QUEST-246, LORE-404).
--
-- Why this exists:
--
--   Migration 005 introduced tasks_fts_rows as the integer-ID bridge for
--   tasks_fts (FTS5 virtual table) and quest_vectors (vector store). It
--   wired a trigger (tasks_fts_status_ai) that fires AFTER INSERT on
--   task_status to register a bridge row. SQLite triggers do not run
--   retroactively against rows that already exist, so installs upgrading
--   from a pre-005 binary kept their N task_status rows but only acquired
--   bridge rows for quests posted post-upgrade.
--
--   On a real install (kunal's, 2026-04-25) the mismatch was 246 distinct
--   task_status.task_id values vs 9 tasks_fts_rows: 96% of quests were
--   silently invisible to embeddings and to the FTS index, and the
--   QUEST-229 auto-backfill saw a coverage of 9/9 = 100% and emitted no
--   warning. quest_search ran arm=bm25 forever; the v0.3 RRF feature
--   shipped dormant.
--
-- What this does:
--
--   1. INSERT OR IGNORE every distinct task_status.task_id into
--      tasks_fts_rows. The IGNORE clause preserves bridge rows that were
--      created post-005 (they keep their existing body content). The
--      tasks_fts_rows_ai trigger fires for every newly inserted row and
--      adds it to the tasks_fts FTS5 index.
--
--   2. For every bridge row whose body is empty, populate body from the
--      concatenated [spec] notes in task_notes (mirroring the body the
--      tasks_fts_notes_ai trigger would have written had the bridge row
--      existed when the note was inserted). This update fires
--      tasks_fts_rows_au which propagates the body change to tasks_fts.
--
-- Idempotency:
--
--   - schema_migrations.version=6 prevents re-execution at the runner
--     level.
--   - INSERT OR IGNORE makes the bridge-row backfill safe to re-run by
--     hand. The body update guards on body = '' so re-running never
--     overwrites a body that the runtime triggers have since populated.
--
-- Non-destructive:
--
--   No DROP, no DELETE, no schema change. Existing rows survive
--   unchanged. Only previously-missing rows are inserted, and only
--   previously-empty bodies are populated.

-- Step 1: bridge every task_status.task_id that is not already in
-- tasks_fts_rows. The trigger tasks_fts_rows_ai propagates each new row
-- into tasks_fts. SELECT DISTINCT in case task_status ever grows a
-- composite key shape; today (project_id, task_id) is the PK so distinct
-- is redundant but cheap.
INSERT OR IGNORE INTO tasks_fts_rows (task_id)
SELECT DISTINCT task_id FROM task_status;

-- Step 2: populate body for any bridge row whose body is still the
-- empty-string default. The body for a quest is the concatenation of
-- every [spec] note in insertion order, separated by a single space.
-- This mirrors the SELECT in the tasks_fts_notes_ai trigger
-- (group_concat(tn.note, ' ') ordered by id). Rows with no [spec] notes
-- keep an empty body; tasks_fts indexes them with no terms which is
-- harmless (they will never match a non-empty query).
UPDATE tasks_fts_rows
SET body = COALESCE((
  SELECT group_concat(tn.note, ' ')
  FROM task_notes tn
  WHERE tn.task_id = tasks_fts_rows.task_id
    AND tn.note LIKE '[spec]%'
), '')
WHERE body = '';

-- Step 3: reset quest.vector_coverage_den so the auto-backfill assess
-- step (and lore_health) picks up the new bridge population on next
-- read. The BackfillOptions ReconcileDen step in internal/lore/embed
-- recomputes this from tasks_fts_rows count when the auto-backfill runs,
-- but resetting here makes the post-migration state immediately accurate
-- without waiting for the next backfill cycle. ON CONFLICT upserts in
-- case the row was missing on a partial-prior-run.
INSERT INTO meta (key, value)
VALUES ('quest.vector_coverage_den', (SELECT CAST(COUNT(*) AS TEXT) FROM tasks_fts_rows))
ON CONFLICT(key) DO UPDATE SET value = excluded.value;
