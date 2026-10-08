-- 011_task_leases.up.sql
--
-- Quest leases: the liveness layer for daemon-mediated claims (ADR-005
-- Part 1, "Why a daemon" item 3; phasing table Phase 3 "leases +
-- heartbeats + presence"). Today a claim is a bare (claimed_by,
-- claimed_at) pair on task_status with no notion of whether the claimant
-- is still alive; a crashed agent's in_progress quest rots as a zombie
-- (the stale-in_progress-claim symptom). A daemon that sees every session can
-- hold a lease with a heartbeat so an expired lease lets a watcher
-- forfeit the stale claim instead of leaving it stuck.
--
--   task_leases  one row per held claim: the project + task it covers,
--                the session that holds it, the human-or-agent holder
--                label, when the lease was acquired, when it was last
--                heartbeated, and when it expires if no further
--                heartbeat lands.
--
-- A SEPARATE table, not new columns on task_status, on purpose:
--   (a) the compare-and-swap claim UPDATE in internal/quest/accept.go and
--       its single-statement auto-commit race contract (the concurrent
--       claim-id-allocation invariant) stay untouched;
--   (b) no-daemon operation writes zero lease rows, so its DB effects
--       remain byte-identical to today. Lease rows are purely additive.
--
-- PRIMARY KEY (project_id, task_id) matches task_status: one live lease
-- per quest. Acquire is INSERT OR REPLACE so a re-acquire (same daemon,
-- new session after a restart) refreshes rather than duplicates.
--
-- This migration is forward-only and additive; it lands cleanly on a
-- fresh DB and on an existing 001-010 DB. The same migrations directory
-- is applied to BOTH lore.db and quest.db (see internal/install/init.go),
-- so the DDL must be harmless in both: quest.db is the canonical home;
-- the copy in lore.db stays inert and empty.
--
-- Idempotent via IF NOT EXISTS, same convention as 007/009/010.

CREATE TABLE IF NOT EXISTS task_leases (
  project_id   TEXT NOT NULL,
  task_id      TEXT NOT NULL,
  session_id   TEXT NOT NULL,
  holder       TEXT NOT NULL,
  acquired_at  TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  PRIMARY KEY (project_id, task_id)
);

-- RenewLeasesForSession / ReleaseLeasesForSession sweep every lease a
-- single session holds; keep that off a full-table scan.
CREATE INDEX IF NOT EXISTS idx_task_leases_session_id
  ON task_leases(session_id);

-- ExpiredLeases scans by expiry to find leases a reaper should forfeit;
-- index the comparison column.
CREATE INDEX IF NOT EXISTS idx_task_leases_expires_at
  ON task_leases(expires_at);
