package lore

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// selectProbeEmbedder is an in-process Embedder a test registers as an
// alternate backend so it can assert WireEmbedDeps selected it by name. It
// records construction so the test can prove the factory ran. No network, no
// model assets.
type selectProbeEmbedder struct{}

func (selectProbeEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]float32, embed.Dim)
	out[0] = 1.0
	return out, nil
}
func (selectProbeEmbedder) Dimension() int { return embed.Dim }

// altBackendBuilt counts how many times the test alternate factory ran, so the
// default-path assertion can prove the alternate factory was NOT invoked.
var altBackendBuilt atomic.Int64

func init() {
	embed.RegisterEmbedder("select-probe-test", func(cfg embed.EmbedConfig) (embed.Embedder, error) {
		altBackendBuilt.Add(1)
		return selectProbeEmbedder{}, nil
	})
}

// TestWireEmbedDeps_SelectsAlternateBackend is the ADR-006 Phase 4 selection
// proof: with meta.embedder_state='enabled' and EmbedWireOptions.Backend set to
// a registered alternate backend, WireEmbedDeps constructs that backend's
// Embedder rather than the local BGE path. No live network: the alternate is an
// in-process stub registered in this file's init().
func TestWireEmbedDeps_SelectsAlternateBackend(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "select-test")

	// Flip meta to enabled with a model id so the state gate passes and the
	// alternate-backend path has a stable model key.
	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "alt-model-x"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	before := altBackendBuilt.Load()
	deps, status, err := WireEmbedDeps(ctx, db, EmbedWireOptions{
		Async:     true,
		LoadIndex: true,
		Backend:   "select-probe-test",
		Model:     "alt-model-x",
	})
	if err != nil {
		t.Fatalf("WireEmbedDeps: %v", err)
	}
	if !status.Wired {
		t.Fatalf("expected Wired=true for a registered alternate backend; reason=%q", status.Reason)
	}
	if deps == nil || deps.Embedder == nil {
		t.Fatal("expected a non-nil EmbedDeps with an Embedder")
	}
	if _, ok := deps.Embedder.(selectProbeEmbedder); !ok {
		t.Fatalf("WireEmbedDeps wired %T, want selectProbeEmbedder (the selected alternate)", deps.Embedder)
	}
	if deps.ModelID != "alt-model-x" {
		t.Errorf("alternate backend ModelID = %q, want alt-model-x", deps.ModelID)
	}
	if got := altBackendBuilt.Load(); got != before+1 {
		t.Errorf("alternate factory ran %d times, want exactly 1", got-before)
	}
}

// TestWireEmbedDeps_DefaultDoesNotUseAlternate proves selection discriminates:
// the default backend (empty) must NOT route through the alternate factory,
// even with meta enabled. On a no-bundled-assets build the default path returns
// the local "no_bundled_assets" reason; the load-bearing assertion is that the
// alternate factory was never invoked.
func TestWireEmbedDeps_DefaultDoesNotUseAlternate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "default-test")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key,value) VALUES ('embedder_state','enabled') ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
	); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	before := altBackendBuilt.Load()
	_, status, err := WireEmbedDeps(ctx, db, EmbedWireOptions{
		Async:     true,
		LoadIndex: true,
		// Backend empty: the default local path.
	})
	if err != nil {
		t.Fatalf("WireEmbedDeps: %v", err)
	}
	// The default path goes to the local BGE manifest checks, which on a
	// default (no -tags=withembed) build reports no_bundled_assets. It must
	// never touch the alternate factory.
	if status.Reason == "enabled" {
		t.Log("default path wired the local embedder (withembed build); acceptable")
	}
	if got := altBackendBuilt.Load(); got != before {
		t.Errorf("default backend invoked the alternate factory %d times, want 0", got-before)
	}
}
