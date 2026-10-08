-- 009_sleep_journal.up.sql
--
-- Durable journal for autonomous maintenance ("sleep") passes, the
-- substrate consumed by internal/sleep. Two tables:
--
--   sleep_passes  one row per pass: when it started/ended, what
--                 triggered it (daemon idle tick vs. degraded
--                 in-process autopass), the wall budget it ran under,
--                 and whether a later session has already narrated it.
--   sleep_ops     one row per mutation (or attempted mutation) inside
--                 a pass: which step produced it, the policy verdict
--                 (auto-applied vs. posted for approval), the op kind,
--                 a display-id target (e.g. "LORE-12<-LORE-40" or
--                 "QUEST-401"), JSON detail, and an optional JSON
--                 inverse payload describing how to manually reverse
--                 an auto-applied op.
--
-- The same migrations directory is applied to BOTH lore.db and
-- quest.db (see internal/install/init.go), so this DDL must land
-- harmlessly in both. lore.db is the canonical home for these tables;
-- the copy created in quest.db stays inert and empty.
--
-- Idempotent via IF NOT EXISTS, same convention as 007.

CREATE TABLE IF NOT EXISTS sleep_passes (
  id          INTEGER PRIMARY KEY,
  started_at  TEXT    NOT NULL,
  ended_at    TEXT,
  "trigger"   TEXT    NOT NULL CHECK ("trigger" IN ('daemon-idle', 'autopass')),
  budget_ms   INTEGER NOT NULL,
  narrated_at TEXT
);

CREATE TABLE IF NOT EXISTS sleep_ops (
  id      INTEGER PRIMARY KEY,
  pass_id INTEGER NOT NULL REFERENCES sleep_passes(id),
  step    TEXT    NOT NULL,
  policy  TEXT    NOT NULL CHECK (policy IN ('auto', 'approval')),
  op      TEXT    NOT NULL,
  target  TEXT    NOT NULL,
  detail  TEXT,
  inverse TEXT,
  applied INTEGER NOT NULL DEFAULT 0
);

-- Narration reads ops per pass; keep that a logarithmic lookup.
CREATE INDEX IF NOT EXISTS idx_sleep_ops_pass_id ON sleep_ops(pass_id);
