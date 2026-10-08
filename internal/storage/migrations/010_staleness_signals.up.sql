-- 010_staleness_signals.up.sql
--
-- Durable staleness observations for lore entries, the persistence seam
-- consumed by internal/lore/staleness.go. Today the git-aware echo check
-- is recomputed on every lore_echoes call (one git subprocess per entry
-- with a file_path); this table lets a long-lived writer (file watcher,
-- scheduled git sweep) record what it observed once, so later reads are
-- subprocess-free.
--
--   staleness_signals  one row per (entry, source) observation: which
--                      entry was flagged, the project it belongs to, a
--                      display-ready reason string, the source that
--                      observed it (e.g. 'watcher-file', 'git-sweep'),
--                      and when it was observed.
--
-- Uniqueness on (entry_id, source) makes repeated observations upsert
-- (refresh reason + observed_at) instead of piling up rows.
--
-- Invalidation is read-side: lore.Echoes only surfaces signals whose
-- entry is still status='current', so reforge/seal/archive transitions
-- retire signals without any write here.
--
-- The same migrations directory is applied to BOTH lore.db and quest.db
-- (see internal/install/init.go), so this DDL must land harmlessly in
-- both. lore.db is the canonical home; the copy in quest.db stays inert
-- and empty.
--
-- Idempotent via IF NOT EXISTS, same convention as 007/009.

CREATE TABLE IF NOT EXISTS staleness_signals (
  entry_id    INTEGER NOT NULL REFERENCES entries(id),
  project_id  TEXT    NOT NULL REFERENCES projects(id),
  reason      TEXT    NOT NULL,
  source      TEXT    NOT NULL,
  observed_at TEXT    NOT NULL,
  PRIMARY KEY (entry_id, source)
);

-- Echoes loads all signals for one project per call; keep that lookup
-- off a full-table scan.
CREATE INDEX IF NOT EXISTS idx_staleness_signals_project_id
  ON staleness_signals(project_id);
