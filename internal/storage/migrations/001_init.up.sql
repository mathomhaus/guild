-- 001_init.up.sql — consolidated baseline schema for guild lore.db and quest.db.
--
-- Single-migration baseline. Carries: projects registry, lore entries + FTS +
-- sync triggers + provenance links, quest task_status + task_notes + task_events,
-- and the hints engine (tables + seeded launch-set rules).
--
-- Applied by internal/storage/migrate.go inside a single transaction per
-- §4 "Schema migrations" and §11.6 "self-heal upgrade".
--
-- NOTE: both lore.db and quest.db run this same migration but only the
-- relevant subset of objects materializes per database in v1 (lore.db gets
-- all of it today; future quests may split the two schemas or keep a
-- shared baseline — tracked by the caller in Migrate's description).

-- ---------------------------------------------------------------------------
-- Shared: projects registry (both lore.db and quest.db carry their own copy
-- of the projects table). quest.py adds a tasks_file column that lore.py
-- omits, so the unified table keeps the superset column and leaves it NULL
-- for lore-only installs.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS projects (
  id         TEXT PRIMARY KEY,
  path       TEXT UNIQUE NOT NULL,
  tasks_file TEXT NOT NULL DEFAULT 'TASKS.md',
  created_at TEXT DEFAULT (datetime('now'))
);

-- ---------------------------------------------------------------------------
-- Lore: entries + entry_links + FTS5 index + sync triggers
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS entries (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id       TEXT NOT NULL,
  topic            TEXT NOT NULL,            -- slug: 'competitive', 'auth', 'context-mgmt'
  kind             TEXT NOT NULL,            -- idea | research | decision | observation | principle
  title            TEXT NOT NULL,
  summary          TEXT NOT NULL,            -- 2-3 sentences, mandatory
  tags             TEXT,                     -- comma-separated semantic tags
  file_path        TEXT,                     -- optional pointer to full content file
  source           TEXT,                     -- URL or reference
  status           TEXT NOT NULL DEFAULT 'current',  -- current|stale|superseded|archived|imported|seed|exploring|promoted|parked
  valid_days       INTEGER,                  -- days before auto-stale (NULL = never)
  needs_review     INTEGER NOT NULL DEFAULT 0,
  prompted_by      TEXT,                     -- quest_id that triggered this
  created_at       TEXT DEFAULT (datetime('now')),
  updated_at       TEXT DEFAULT (datetime('now')),
  access_count     INTEGER NOT NULL DEFAULT 0,
  last_accessed_at TEXT,
  FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE TABLE IF NOT EXISTS entry_links (
  from_id    INTEGER NOT NULL,
  to_id      INTEGER NOT NULL,
  relation   TEXT NOT NULL,                  -- informs | supersedes | contradicts
  created_at TEXT DEFAULT (datetime('now')),
  PRIMARY KEY (from_id, to_id),
  FOREIGN KEY (from_id) REFERENCES entries(id),
  FOREIGN KEY (to_id)   REFERENCES entries(id)
);

-- FTS5 virtual table for full-text search over title, summary, tags.
-- content=entries + content_rowid=id makes this a "contentless-shadow" index
-- that stores no copy of the content but follows entries.id as the rowid.
CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
  title, summary, tags,
  content=entries, content_rowid=id
);

-- FTS5 sync triggers. Scoped to title/summary/tags so access_count /
-- last_accessed_at updates don't re-index the row on every read.
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

-- ---------------------------------------------------------------------------
-- Quest: task_status + task_notes + task_events
--
-- task_status is the authoritative state table (one row per (project, task)).
-- task_notes and task_events are append-only logs the quest CLI uses for
-- journal/pulse/coordination output.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS task_status (
  project_id  TEXT NOT NULL,
  task_id     TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'next',  -- next|in_progress|done|blocked
  claimed_by  TEXT,
  claimed_at  TEXT,
  updated_at  TEXT DEFAULT (datetime('now')),
  PRIMARY KEY (project_id, task_id),
  FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE TABLE IF NOT EXISTS task_notes (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id  TEXT NOT NULL,
  task_id     TEXT NOT NULL,
  agent_id    TEXT NOT NULL,
  note        TEXT NOT NULL,
  created_at  TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS task_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id  TEXT NOT NULL,
  task_id     TEXT NOT NULL,
  event       TEXT NOT NULL,
  agent_id    TEXT,
  data        TEXT,
  created_at  TEXT DEFAULT (datetime('now'))
);

-- ---------------------------------------------------------------------------
-- Hints engine: rule registry + fire log + seeded launch-set rules.
--
--   hints       — one row per rule (enabled state, severity, cooldown, etc.)
--   hint_fires  — append-only log of every evaluation that produced a fire,
--                 with a follow-through column the engine scores asynchronously
--                 after N subsequent guild tool calls.
--
-- Schema home: both lore.db and quest.db run this migration so hint tables
-- materialize on both. The engine writes to quest.db because it carries the
-- session-activity log (task_events) the follow-through scorer is kin with.
-- The lore.db copy is inert but harmless.
--
-- Seed set: 9 launch-set rules (6 💡 hint + 3 ℹ️ fyi). INSERT OR IGNORE so
-- reruns are safe and operator edits to enabled/template survive.
--
-- no-brief-24h carries a per_era_severity payload so the engine demotes to
-- ℹ️ fyi in the Bash-CLI era (where the rule hit the auto-prune floor on
-- CLI traffic vs MCP traffic).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS hints (
  id                  INTEGER PRIMARY KEY,
  rule_id             TEXT UNIQUE NOT NULL,          -- stable string id
  trigger_tool        TEXT NOT NULL,                 -- tool name that triggers evaluation
  severity            TEXT NOT NULL,                 -- blocker|warning|hint|fyi
  template            TEXT NOT NULL,                 -- hint message template
  cooldown_calls      INTEGER NOT NULL DEFAULT 5,    -- no re-fire within N calls (same rule)
  per_era_severity    TEXT,                          -- optional JSON: {"mcp":"hint","bash":"fyi"}
  enabled             INTEGER NOT NULL DEFAULT 1,    -- 0 = auto-disabled by prune
  created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS hint_fires (
  id                      INTEGER PRIMARY KEY,
  rule_id                 TEXT NOT NULL,
  tool_call_id            TEXT,
  session_id              TEXT,
  fired_at                TEXT NOT NULL DEFAULT (datetime('now')),
  followed_through        INTEGER,                   -- NULL=unscored, 0=miss, 1=hit
  followup_event_offset   INTEGER                    -- how many events later the hit landed
);

CREATE INDEX IF NOT EXISTS idx_hint_fires_rule    ON hint_fires(rule_id, fired_at);
CREATE INDEX IF NOT EXISTS idx_hint_fires_session ON hint_fires(session_id, fired_at);
CREATE INDEX IF NOT EXISTS idx_hint_fires_pending ON hint_fires(followed_through, fired_at) WHERE followed_through IS NULL;

INSERT OR IGNORE INTO hints (rule_id, trigger_tool, severity, template, cooldown_calls, per_era_severity) VALUES
  ('inscribe-looks-like-quest',   'lore_inscribe', 'hint', 'title/summary mentions work to do — consider quest_post(subject=…) for TODOs instead of lore_inscribe',                                   5, NULL),
  ('no-session-start',            '*',             'hint', 'no guild_session_start yet this session — call guild_session_start() to load briefing/oath/top-bounty before operating',                   5, NULL),
  ('session-end-without-brief',   '*',             'hint', 'session is long (30+ guild calls) with no quest_brief — consider quest_brief("what was done, what''s next") before compact',             10, NULL),
  ('slug-query',                  'lore_appraise', 'hint', 'query looks slug-like — did you mean quest_list or quest_scroll?',                                                                        5, NULL),
  ('journal-outside-accepted',    'quest_journal', 'hint', 'journaling on a quest not accepted this session — accept it first (quest_accept) or file a fresh quest_post',                             5, NULL),
  ('no-brief-24h',                'quest_fulfill', 'hint', 'no quest_brief yet this session — consider quest_brief("what was done, what''s next") before compact',                                    5, '{"mcp":"hint","bash":"fyi"}'),
  ('inscribe-without-appraise',   'lore_inscribe', 'fyi',  'no lore_appraise in the last 5 calls — consider appraise(query="…", all_projects=true) before inscribing to avoid duplicates',            5, NULL),
  ('clear-without-report-detail', 'quest_fulfill', 'fyi',  'report is short (<20 words) — a richer report (commit hash, files, follow-ups) makes cold-start handoff cleaner',                         5, NULL),
  ('principle-too-long',          'lore_inscribe', 'fyi',  'principle exceeds the ≤60-word oath-hygiene target — consider kind=decision for long rationale and re-inscribe a shorter principle',      5, NULL);
