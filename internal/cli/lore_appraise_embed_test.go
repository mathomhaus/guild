package cli

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// Deterministic embeddings prove CLI/index wiring, not semantic relevance.
// The query shares no FTS vocabulary with the stored evidence, so a printed
// entry proves the command reached vector retrieval rather than echoing input.
func TestCLIAppraiseLoadsSearchIndex(t *testing.T) {
	db, _ := cliSetup(t, "alpha")
	t.Setenv("GUILD_NO_USAGE_LOG", "1")
	t.Setenv("GUILD_NO_EMOJI", "1")
	ctx := context.Background()
	for _, kv := range [][2]string{{"embedder_state", "enabled"}, {"embedder_model_id", "cli-wiring"}} {
		if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key,value)VALUES(?,?)`, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	r, err := db.ExecContext(ctx, `INSERT INTO entries(project_id,topic,kind,title,summary,status)VALUES('alpha','test','decision','Opaque stored evidence','Persisted design rationale','current')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	e := embed.NewDeterministicEmbedder()
	if _, err := embed.WriteVector(ctx, db, embed.HotDeps{Embedder: e, ModelID: "cli-wiring"}, id, "Persisted design rationale"); err != nil {
		t.Fatal(err)
	}
	originalWire := wireAppraiseEmbedDeps
	t.Cleanup(func() { wireAppraiseEmbedDeps = originalWire })
	for _, hasIndex := range []bool{true, false} {
		wireAppraiseEmbedDeps = func(ctx context.Context, db *sql.DB, opts lore.EmbedWireOptions) (*lore.EmbedDeps, lore.WireEmbedStatus, error) {
			if !opts.LoadIndex || opts.Async {
				t.Fatalf("CLI search did not request its synchronous index: %+v", opts)
			}
			var idx *embed.Index
			if hasIndex {
				idx = embed.NewIndex(embed.LoreCorpus{}, "cli-wiring")
				if _, err := idx.LoadFromDB(ctx, db); err != nil {
					return nil, lore.WireEmbedStatus{}, err
				}
			}
			return &lore.EmbedDeps{Embedder: e, Index: idx, ModelID: "cli-wiring"}, lore.WireEmbedStatus{Wired: true}, nil
		}
		cmd := newAppraiseCmd()
		buf := &bytes.Buffer{}
		cmd.SetOut(buf)
		cmd.SetErr(buf)
		cmd.SetArgs([]string{"enigmatic", "retrieval"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		returnedEvidence := strings.Contains(buf.String(), "Opaque stored evidence")
		if returnedEvidence != hasIndex {
			t.Fatalf("vector-only query returnedEvidence=%v with hasIndex=%v: %s", returnedEvidence, hasIndex, buf)
		}
	}
}
