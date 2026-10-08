package lore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/storage"
)

func TestEmbeddingHealthReportsBothCorpora(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	paths := map[string]string{"lore": filepath.Join(dir, "lore.db"), "quest": filepath.Join(dir, "quest.db")}
	for corpus, path := range paths {
		db, err := storage.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.MigrateTo(ctx, db, corpus, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO projects(id,path) VALUES('fixture','/fixture')`); err != nil {
			t.Fatal(err)
		}
		if corpus == "lore" {
			if _, err := db.Exec(`INSERT INTO entries(project_id,topic,kind,title,summary) VALUES('fixture','test','observation','title','source')`); err != nil {
				t.Fatal(err)
			}
			if _, err := embed.Backfill(ctx, embed.BackfillOptions{DB: db, Embedder: embed.NewDeterministicEmbedder(), ModelID: "bge-small-en-v1.5-int8-cls"}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := db.Exec(`INSERT INTO task_status(project_id,task_id) VALUES('fixture','task'); INSERT INTO task_notes(project_id,task_id,agent_id,note) VALUES('fixture','task','fixture','[spec] subject: missing quest vector')`); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	deps := command.Deps{
		OpenDB:      func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, paths["lore"]) },
		OpenQuestDB: func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, paths["quest"]) },
		ResolveProj: func(context.Context, string) (string, error) { return "fixture", nil },
	}
	out, err := EmbedderHealthCommand.Handler(ctx, deps, EmbedderHealthInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Report.FreshCount != 1 || out.Report.CoverageDen != 1 || out.QuestReport == nil || out.QuestReport.PendingCount != 1 || out.QuestReport.FreshCount != 0 {
		t.Fatalf("combined health=%+v quest=%+v", out.Report, out.QuestReport)
	}
	rendered := EmbedderHealthCommand.MCPFormat(command.MCPSink{}, out)
	if !strings.Contains(rendered, "lore embedder section") || !strings.Contains(rendered, "quest embedder section") || strings.Contains(rendered, "ETA") || strings.Contains(rendered, "backfilling") {
		t.Fatalf("combined health text=%s", rendered)
	}
}
