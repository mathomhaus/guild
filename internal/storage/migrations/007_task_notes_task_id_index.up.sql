-- 007_task_notes_task_id_index.up.sql
--
-- Adds an index on task_notes(task_id) to make QuestCorpus.SourceText
-- a logarithmic lookup rather than a full table scan. Without this,
-- each backfill cycle iterates pending entities and runs SourceText
-- per entity; on installs with O(thousands) of task_notes, that is a
-- multi-million-row workload per cycle (LORE-416).
--
-- Idempotent via IF NOT EXISTS.

CREATE INDEX IF NOT EXISTS idx_task_notes_task_id ON task_notes(task_id);
