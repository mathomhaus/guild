package quest

import (
	"context"
	"math"
	"testing"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

type malformedQuestQueryEmbedder struct {
	value float32
}

func (malformedQuestQueryEmbedder) Dimension() int { return embed.Dim }
func (e malformedQuestQueryEmbedder) Embed(context.Context, string) ([]float32, error) {
	v := make([]float32, embed.Dim)
	v[0] = e.value
	return v, nil
}

func TestQuestSearch_InvalidQueryVectorFallsBackToBM25(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value float32
	}{
		{"zero", 0},
		{"nan", float32(math.NaN())},
		{"positive_infinity", float32(math.Inf(1))},
		{"negative_infinity", float32(math.Inf(-1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, pid := newTestDB(t)
			lexical := mustPost(t, db, pid, PostParams{Subject: "validatedkeyword lexical answer"})
			mustPost(t, db, pid, PostParams{Subject: "unrelated semantic candidate"})
			deps := seedSearchVectors(t, db, pid)
			deps.Embedder = malformedQuestQueryEmbedder{value: tc.value}
			out, err := RunQuestSearchForProject(context.Background(), db, "validatedkeyword", 10, pid, deps)
			if err != nil || out.Arm != "bm25" || len(out.Results) != 1 || out.Results[0].QuestID != lexical.ID {
				t.Fatalf("malformed query vector contributed arbitrary semantic hits: out=%+v err=%v", out, err)
			}
		})
	}
}
