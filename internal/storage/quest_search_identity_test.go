package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestQuestSearchIdentityMigration(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical=%v", historical), func(t *testing.T) {
			ctx := context.Background()
			db := openFreshDB(t)
			if historical {
				// Apply the actual pre-identity schema, not a mock of its tables.
				old := make(fstest.MapFS)
				files, err := fs.ReadDir(migrationFS, "migrations")
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range files {
					if strings.HasPrefix(f.Name(), "014_") {
						continue
					}
					name := "migrations/" + f.Name()
					data, err := fs.ReadFile(migrationFS, name)
					if err != nil {
						t.Fatal(err)
					}
					old[name] = &fstest.MapFile{Data: data}
				}
				if err := MigrateFS(ctx, db, old, "", io.Discard); err != nil {
					t.Fatal(err)
				}
			} else if err := MigrateTo(ctx, db, "", io.Discard); err != nil {
				t.Fatal(err)
			}

			const projects = 70
			for i := 0; i < projects; i++ {
				pid := fmt.Sprintf("project-%02d", i)
				if _, err := db.ExecContext(ctx, `INSERT INTO projects(id,path) VALUES (?,?)`, pid, "/tmp/"+pid); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO task_status(project_id,task_id,status) VALUES (?,'QUEST-1','done')`, pid); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO task_notes(project_id,task_id,agent_id,note) VALUES (?,'QUEST-1','test',?)`, pid, "[spec] subject: isolated "+pid); err != nil {
					t.Fatal(err)
				}
			}
			if historical {
				var n int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks_fts_rows`).Scan(&n); err != nil || n != 1 {
					t.Fatalf("historical collision: count=%d err=%v", n, err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO quest_vectors(entry_id,model_id,dim,vec,encoded_at,content_hash) SELECT id,'test-model',384,zeroblob(384),0,'mixed-project-content' FROM tasks_fts_rows`); err != nil {
					t.Fatal(err)
				}
			}
			if err := MigrateTo(ctx, db, "", io.Discard); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"task_status", "task_notes", "tasks_fts_rows"} {
				var n int
				if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != projects { //nolint:sqlcheck,gosec // table comes from a fixed test-owned list
					t.Fatalf("%s count=%d err=%v", table, n, err)
				}
			}
			var vectors int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quest_vectors`).Scan(&vectors); err != nil || vectors != 0 {
				t.Fatalf("legacy vectors survived: count=%d err=%v", vectors, err)
			}
			for i := 0; i < projects; i++ {
				pid := fmt.Sprintf("project-%02d", i)
				var body string
				if err := db.QueryRowContext(ctx, `SELECT body FROM tasks_fts_rows WHERE project_id=? AND task_id='QUEST-1'`, pid).Scan(&body); err != nil || body != "[spec] subject: isolated "+pid {
					t.Fatalf("body for %s=%q err=%v", pid, body, err)
				}
			}
			var den string
			if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='quest.vector_coverage_den'`).Scan(&den); err != nil || den != "70" {
				t.Fatalf("den=%s err=%v", den, err)
			}
			var matches int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks_fts WHERE tasks_fts MATCH 'isolated'`).Scan(&matches); err != nil || matches != projects {
				t.Fatalf("rebuilt FTS matches=%d err=%v", matches, err)
			}
			// Once applied, startup must preserve new vector rows and bridge IDs.
			if _, err := db.ExecContext(ctx, `INSERT INTO quest_vectors(entry_id,model_id,dim,vec,encoded_at,content_hash) SELECT id,'test-model',384,zeroblob(384),0,'new-content' FROM tasks_fts_rows LIMIT 1`); err != nil {
				t.Fatal(err)
			}
			if err := MigrateTo(ctx, db, "", io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quest_vectors`).Scan(&vectors); err != nil || vectors != 1 {
				t.Fatalf("idempotent startup lost rebuilt vector: count=%d err=%v", vectors, err)
			}
			rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if rows.Next() {
				t.Fatal("quest identity migration broke foreign keys")
			}
		})
	}
}
