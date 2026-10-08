-- Project-qualified quest search identity. Rebuild derived search data
-- from canonical status and spec notes; no underlying quest records change.
-- Old vectors may contain text from several projects, so none are trusted.
-- The migration runner executes this transaction once; startup replays are
-- harmless because schema_migrations records version 014 atomically.

DROP TRIGGER IF EXISTS tasks_fts_status_ai;
DROP TRIGGER IF EXISTS tasks_fts_notes_ai;
DROP TRIGGER IF EXISTS tasks_fts_rows_ai;
DROP TRIGGER IF EXISTS tasks_fts_rows_au;
DROP TRIGGER IF EXISTS tasks_fts_rows_ad;
DROP TABLE quest_vectors;
DROP TABLE tasks_fts;
DROP TABLE tasks_fts_rows;

CREATE TABLE tasks_fts_rows (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  task_id    TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  UNIQUE (project_id, task_id),
  FOREIGN KEY (project_id, task_id)
    REFERENCES task_status(project_id, task_id) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_task_notes_project_task
  ON task_notes(project_id, task_id, id);

INSERT INTO tasks_fts_rows (project_id, task_id, body)
SELECT ts.project_id, ts.task_id, COALESCE((
  SELECT group_concat(note, char(10)) FROM (
    SELECT tn.note FROM task_notes tn
    WHERE tn.project_id = ts.project_id AND tn.task_id = ts.task_id
      AND (tn.note LIKE '[spec]%' OR tn.note LIKE '[spec-replace]%')
    ORDER BY tn.id
  )
), '')
FROM task_status ts ORDER BY ts.project_id, ts.task_id;

CREATE VIRTUAL TABLE tasks_fts USING fts5(
  body, content=tasks_fts_rows, content_rowid=id,
  tokenize = 'porter unicode61 remove_diacritics 1'
);
INSERT INTO tasks_fts(tasks_fts) VALUES ('rebuild');

CREATE TABLE quest_vectors (
  entry_id     INTEGER PRIMARY KEY REFERENCES tasks_fts_rows(id) ON DELETE CASCADE,
  model_id     TEXT NOT NULL,
  dim          INTEGER NOT NULL,
  vec          BLOB NOT NULL,
  encoded_at   INTEGER NOT NULL,
  content_hash TEXT NOT NULL
);

UPDATE meta SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
WHERE key = 'quest.vector_epoch';
UPDATE meta SET value = '0' WHERE key = 'quest.vector_coverage_num';
UPDATE meta SET value = (SELECT CAST(COUNT(*) AS TEXT) FROM tasks_fts_rows WHERE body <> '')
WHERE key = 'quest.vector_coverage_den';

CREATE TRIGGER tasks_fts_rows_ai AFTER INSERT ON tasks_fts_rows BEGIN
  INSERT INTO tasks_fts(rowid, body) VALUES (new.id, new.body);
  UPDATE meta SET value = (SELECT CAST(COUNT(*) AS TEXT) FROM tasks_fts_rows WHERE body <> '')
  WHERE key = 'quest.vector_coverage_den';
END;

CREATE TRIGGER tasks_fts_rows_au AFTER UPDATE OF body ON tasks_fts_rows
WHEN new.body <> old.body
BEGIN
  INSERT INTO tasks_fts(tasks_fts, rowid, body) VALUES ('delete', old.id, old.body);
  INSERT INTO tasks_fts(rowid, body) VALUES (new.id, new.body);
  DELETE FROM quest_vectors WHERE entry_id = old.id;
  UPDATE meta SET value = (SELECT CAST(COUNT(*) AS TEXT) FROM tasks_fts_rows WHERE body <> '')
  WHERE key = 'quest.vector_coverage_den';
  UPDATE meta SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
  WHERE key = 'quest.vector_epoch';
  UPDATE meta SET value = (SELECT CAST(COUNT(*) AS TEXT) FROM quest_vectors
    WHERE model_id = (SELECT value FROM meta WHERE key = 'quest.embedder_model_id')
      AND dim = 384 AND length(vec) = 384)
  WHERE key = 'quest.vector_coverage_num';
END;

CREATE TRIGGER tasks_fts_rows_ad AFTER DELETE ON tasks_fts_rows BEGIN
  INSERT INTO tasks_fts(tasks_fts, rowid, body) VALUES ('delete', old.id, old.body);
  DELETE FROM quest_vectors WHERE entry_id = old.id;
  UPDATE meta SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
  WHERE key = 'quest.vector_epoch';
  UPDATE meta SET value = (SELECT CAST(COUNT(*) AS TEXT) FROM tasks_fts_rows WHERE body <> '')
  WHERE key = 'quest.vector_coverage_den';
  UPDATE meta SET value = (SELECT CAST(COUNT(*) AS TEXT) FROM quest_vectors
    WHERE model_id = (SELECT value FROM meta WHERE key = 'quest.embedder_model_id')
      AND dim = 384 AND length(vec) = 384)
  WHERE key = 'quest.vector_coverage_num';
END;

CREATE TRIGGER tasks_fts_status_ai AFTER INSERT ON task_status BEGIN
  INSERT OR IGNORE INTO tasks_fts_rows (project_id, task_id, body)
  VALUES (new.project_id, new.task_id, COALESCE((
    SELECT group_concat(note, char(10)) FROM (
      SELECT tn.note FROM task_notes tn
      WHERE tn.project_id = new.project_id AND tn.task_id = new.task_id
        AND (tn.note LIKE '[spec]%' OR tn.note LIKE '[spec-replace]%')
      ORDER BY tn.id
    )
  ), ''));
END;

-- Explicit deletion also works for connections with foreign_keys disabled.
CREATE TRIGGER tasks_fts_status_ad AFTER DELETE ON task_status BEGIN
  DELETE FROM tasks_fts_rows
  WHERE project_id = old.project_id AND task_id = old.task_id;
END;

CREATE TRIGGER tasks_fts_notes_ai AFTER INSERT ON task_notes
WHEN new.note LIKE '[spec]%' OR new.note LIKE '[spec-replace]%'
BEGIN
  UPDATE tasks_fts_rows SET body = COALESCE((
    SELECT group_concat(note, char(10)) FROM (
      SELECT tn.note FROM task_notes tn
      WHERE tn.project_id = new.project_id AND tn.task_id = new.task_id
        AND (tn.note LIKE '[spec]%' OR tn.note LIKE '[spec-replace]%')
      ORDER BY tn.id
    )
  ), '')
  WHERE project_id = new.project_id AND task_id = new.task_id;
END;

CREATE TRIGGER tasks_fts_notes_ad AFTER DELETE ON task_notes
WHEN old.note LIKE '[spec]%' OR old.note LIKE '[spec-replace]%'
BEGIN
  UPDATE tasks_fts_rows SET body = COALESCE((
    SELECT group_concat(note, char(10)) FROM (
      SELECT tn.note FROM task_notes tn
      WHERE tn.project_id = old.project_id AND tn.task_id = old.task_id
        AND (tn.note LIKE '[spec]%' OR tn.note LIKE '[spec-replace]%')
      ORDER BY tn.id
    )
  ), '')
  WHERE project_id = old.project_id AND task_id = old.task_id;
END;

CREATE TRIGGER tasks_fts_notes_au AFTER UPDATE OF project_id, task_id, note ON task_notes
WHEN old.note LIKE '[spec]%' OR old.note LIKE '[spec-replace]%'
  OR new.note LIKE '[spec]%' OR new.note LIKE '[spec-replace]%'
BEGIN
  UPDATE tasks_fts_rows SET body = COALESCE((
    SELECT group_concat(note, char(10)) FROM (
      SELECT tn.note FROM task_notes tn
      WHERE tn.project_id = old.project_id AND tn.task_id = old.task_id
        AND (tn.note LIKE '[spec]%' OR tn.note LIKE '[spec-replace]%')
      ORDER BY tn.id
    )
  ), '')
  WHERE project_id = old.project_id AND task_id = old.task_id;
  UPDATE tasks_fts_rows SET body = COALESCE((
    SELECT group_concat(note, char(10)) FROM (
      SELECT tn.note FROM task_notes tn
      WHERE tn.project_id = new.project_id AND tn.task_id = new.task_id
        AND (tn.note LIKE '[spec]%' OR tn.note LIKE '[spec-replace]%')
      ORDER BY tn.id
    )
  ), '')
  WHERE project_id = new.project_id AND task_id = new.task_id;
END;
