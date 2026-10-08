//go:build unix

package eval

import (
	"context"
	"encoding/json"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"os"
	"testing"
)

func TestRelevanceRealModel(t *testing.T) {
	vocab, lib, model := os.Getenv("GUILD_EMBED_TEST_VOCAB"), os.Getenv("GUILD_EMBED_TEST_LIB"), os.Getenv("GUILD_EMBED_TEST_MODEL")
	if vocab == "" || lib == "" || model == "" {
		t.Skip("real-model relevance NOT validated: set GUILD_EMBED_TEST_VOCAB/_LIB/_MODEL")
	}
	e, err := embed.NewBGEEmbedder(embed.RuntimeConfig{LibraryPath: lib, ModelPath: model, VocabPath: vocab, NumThreads: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, mode := range []string{"vector", "hybrid"} {
		r, err := RunRelevance(context.Background(), RelevanceOptions{Mode: mode, Embedder: e, ModelID: "eval-bge-small-en-v1.5"})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r)
		t.Log(string(b))
		// Held-out paraphrases must be recalled. Negatives remain unverified
		// candidates; their false-positive candidate rate is reported explicitly.
		if r.Metrics.HitAt5 < 0.85 {
			t.Errorf("%s held-out hit@5=%g, require >=0.85", mode, r.Metrics.HitAt5)
		}
	}
	prefixed, err := RunRelevance(context.Background(), RelevanceOptions{Mode: "vector", Embedder: e, ModelID: "eval-bge-small-en-v1.5", QueryPrefix: "Represent this sentence for searching relevant passages: "})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(prefixed)
	t.Log("prefix experiment (no default change): " + string(b))
}
