// Bundled-manifest parity + adversarial coverage. Compiled only under
// `-tags=withembed` because it depends on the per-platform assets.
//
// Two tests live here:
//
//  1. TestEmbeddingParity_Bundled: exercises the SAME extract + embedder
//     construction sequence guild init runs, then verifies every
//     reference string cosine-matches the checked-in fixture at the
//     probe floor or better. This is the regression test QUEST-215
//     needed: the env-var parity test in parity_test.go was passing
//     against the spike's fp32 assets while the bundled int8 path was
//     silently producing 0.98 cosine. Running the bundled manifest here
//     catches that drift before a user does.
//
//  2. TestProbe_AdversarialRejectsUnrelated: the probe reference vector
//     for "retry logic with exponential backoff" must NOT cosine-match
//     an UNRELATED string at the probe floor. This guards against a
//     future loosening of ProbeMinCosine that would silently let a
//     wrong-model or corrupted-output scenario through.

//go:build withembed && unix

package embed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddingParity_Bundled runs the bundled manifest through the
// production Extract + BGEEmbedder path and asserts every fixture entry
// matches its reference within minCosine.
func TestEmbeddingParity_Bundled(t *testing.T) {
	if !HasAssets() {
		t.Skip("build lacks bundled assets (-tags=withembed on an unsupported triple)")
	}
	man := CurrentManifest()
	if len(man.Assets) < 3 {
		t.Fatalf("bundled manifest has %d assets; want 3", len(man.Assets))
	}

	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, man.Identity.PlatformTag)
	ext, err := Extract(man, cacheDir)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Verify extracted bytes match the in-binary SHA, so this test also
	// doubles as a check that the go:embed bytes survived the extract
	// cycle intact (mtime drift, permission scrubbing, atomic rename).
	for i, a := range man.Assets {
		path := ""
		switch i {
		case AssetLibrary:
			path = ext.LibraryPath
		case AssetModel:
			path = ext.ModelPath
		case AssetVocab:
			path = ext.VocabPath
		}
		got, err := fileSHA256Hex(path)
		if err != nil {
			t.Fatalf("sha %s: %v", path, err)
		}
		if got != a.SHA256 {
			t.Fatalf("extracted sha mismatch for %s: got %s want %s", a.Name, got, a.SHA256)
		}
	}

	emb, err := NewBGEEmbedder(RuntimeConfig{
		LibraryPath: ext.LibraryPath,
		ModelPath:   ext.ModelPath,
		VocabPath:   ext.VocabPath,
		NumThreads:  1, // Match the probe + fixture-generator thread count.
	})
	if err != nil {
		t.Fatalf("NewBGEEmbedder: %v", err)
	}
	defer emb.Close()

	// Load the checked-in fixture directly here; do not reuse
	// loadReferenceVectors from parity_test.go because this test runs
	// regardless of env-var availability.
	data, err := os.ReadFile(filepath.Join("testdata", "reference_vectors.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ref := make(map[string]refEntry)
	if err := json.Unmarshal(data, &ref); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(ref) == 0 {
		t.Fatal("fixture is empty")
	}

	ctx := context.Background()
	worst := 1.0
	for text, want := range ref {
		got, err := emb.Embed(ctx, text)
		if err != nil {
			t.Fatalf("Embed(%q): %v", text, err)
		}
		if len(got) != Dim {
			t.Errorf("Embed(%q) returned %d-dim; want %d", text, len(got), Dim)
			continue
		}
		sim := cosine(got, want.Embedding)
		if sim < worst {
			worst = sim
		}
		if sim < minCosine {
			t.Errorf("bundled cosine for %q: %.6f (want >= %.6f)", text, sim, minCosine)
		} else {
			t.Logf("bundled parity %q: cosine=%.6f", text, sim)
		}
	}
	t.Logf("bundled worst cosine across %d entries: %.6f (floor %.6f)", len(ref), worst, minCosine)
}

// TestProbe_AdversarialRejectsUnrelated confirms that an UNRELATED
// string does not spuriously match the probe reference vector at the
// probe floor. This is the guardrail that lets us sleep at night after
// any future tolerance change: even if ProbeMinCosine drops, the
// reference vector for "retry logic with exponential backoff" still has
// to be topically distinct from obviously different inputs.
func TestProbe_AdversarialRejectsUnrelated(t *testing.T) {
	if !HasAssets() {
		t.Skip("build lacks bundled assets (-tags=withembed on an unsupported triple)")
	}
	man := CurrentManifest()
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, man.Identity.PlatformTag)
	ext, err := Extract(man, cacheDir)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	emb, err := NewBGEEmbedder(RuntimeConfig{
		LibraryPath: ext.LibraryPath,
		ModelPath:   ext.ModelPath,
		VocabPath:   ext.VocabPath,
		NumThreads:  1,
	})
	if err != nil {
		t.Fatalf("NewBGEEmbedder: %v", err)
	}
	defer emb.Close()

	ref, err := loadReferenceEmbedding(ProbeString)
	if err != nil {
		t.Fatalf("loadReferenceEmbedding: %v", err)
	}

	// Adversarial inputs: topically unrelated to the probe string.
	// Each must land well below the probe floor against the probe's
	// reference vector. The 0.9 ceiling is conservative: empirical
	// cross-topic cosines for bge-small sit in the 0.3-0.7 range on
	// English sentences, so 0.9 leaves a huge margin while still
	// catching a wrong-model scenario (wrong model + same tokenizer ->
	// near-uniform vectors that could spuriously score high).
	const adversarialCeiling = 0.9
	adversarial := []string{
		"photosynthesis converts sunlight into chemical energy in plants",
		"the symphony orchestra performed beethoven's ninth last night",
		"baking sourdough bread requires a live starter culture",
	}
	ctx := context.Background()
	for _, text := range adversarial {
		v, err := emb.Embed(ctx, text)
		if err != nil {
			t.Fatalf("Embed(%q): %v", text, err)
		}
		sim := cosineSimilarity(v, ref)
		if sim >= adversarialCeiling {
			t.Errorf("adversarial cosine for %q: %.6f (want < %.6f); probe reference does not discriminate",
				text, sim, adversarialCeiling)
		} else {
			t.Logf("adversarial %q: cosine=%.6f", text, sim)
		}
	}
}
