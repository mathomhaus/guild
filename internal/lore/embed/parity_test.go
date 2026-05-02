// BGE parity tests. Two separate tests:
//
//  1. TestTokenizerParity: pure-Go, verifies that our WordPiece
//     tokenizer emits the same input_ids / attention_mask / token_type_ids
//     as the Python HuggingFace BertTokenizer for bge-small-en-v1.5.
//     Requires a vocab.txt; skips if not reachable.
//
//  2. TestEmbeddingParity: loads libonnxruntime + model.onnx, runs each
//     reference string through BGEEmbedder, and asserts cosine >= 0.999
//     against the pinned Python-reference vector. Skips if any of the
//     three artifacts are not reachable so the test file is safe to
//     compile in CI without the full ONNX stack installed.
//
// Paths come from three environment variables so this test is runnable
// without moving the ~65MB ORT + model assets into the repo:
//
//	GUILD_EMBED_TEST_VOCAB   absolute path to vocab.txt
//	GUILD_EMBED_TEST_LIB     absolute path to libonnxruntime.{dylib,so}
//	GUILD_EMBED_TEST_MODEL   absolute path to model.onnx
//
// Unset variables mean "skip the test" (not fail). The reference JSON
// fixture itself is checked in at testdata/reference_vectors.json so the
// tokenizer-parity test runs anywhere the vocab is reachable.

//go:build unix

package embed

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// minCosine is the acceptance floor from QUEST-207. Spike runs came in
// at >=0.9995 across the reference corpus; we use 0.999 to give a small
// margin for ORT minor-version numeric drift within the 1.23.x line.
const minCosine = 0.999

type refEntry struct {
	InputIDs      []int64   `json:"input_ids"`
	AttentionMask []int64   `json:"attention_mask"`
	TokenTypeIDs  []int64   `json:"token_type_ids"`
	Embedding     []float32 `json:"embedding"`
}

func loadReferenceVectors(t *testing.T) map[string]refEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "reference_vectors.json"))
	if err != nil {
		t.Fatalf("read reference_vectors.json fixture: %v", err)
	}
	var m map[string]refEntry
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse reference_vectors.json fixture: %v", err)
	}
	if len(m) == 0 {
		t.Fatal("reference_vectors.json fixture is empty")
	}
	return m
}

// testVocabPath returns a path to vocab.txt or "" if none is reachable.
// We consult GUILD_EMBED_TEST_VOCAB first, then fall back to the spike's
// workspace path so the test is runnable on a dev laptop with the spike
// checked out alongside guild.
func testVocabPath() string {
	if p := os.Getenv("GUILD_EMBED_TEST_VOCAB"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func testLibPath() string {
	if p := os.Getenv("GUILD_EMBED_TEST_LIB"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func testModelPath() string {
	if p := os.Getenv("GUILD_EMBED_TEST_MODEL"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestTokenizerParity verifies our Go WordPiece emits bit-identical
// input_ids / attention_mask / token_type_ids to the Python reference
// BertTokenizer for bge-small-en-v1.5.
func TestTokenizerParity(t *testing.T) {
	vocabPath := testVocabPath()
	if vocabPath == "" {
		t.Skip("GUILD_EMBED_TEST_VOCAB not set or path not readable; skipping tokenizer parity")
	}
	vocab, err := LoadVocab(vocabPath)
	if err != nil {
		t.Fatalf("LoadVocab(%q): %v", vocabPath, err)
	}
	tk := NewWordPieceTokenizer(vocab)
	if err := tk.assertVocabHasSpecials(); err != nil {
		t.Fatalf("vocab specials: %v", err)
	}
	ref := loadReferenceVectors(t)
	for text, want := range ref {
		ids, mask, typeIDs := tk.Encode(text, 512)
		if !equalInt64(ids, want.InputIDs) {
			t.Errorf("input_ids mismatch for %q\n  got:  %v\n  want: %v", text, ids, want.InputIDs)
			continue
		}
		if !equalInt64(mask, want.AttentionMask) {
			t.Errorf("attention_mask mismatch for %q\n  got:  %v\n  want: %v", text, mask, want.AttentionMask)
		}
		if !equalInt64(typeIDs, want.TokenTypeIDs) {
			t.Errorf("token_type_ids mismatch for %q\n  got:  %v\n  want: %v", text, typeIDs, want.TokenTypeIDs)
		}
	}
}

// TestEmbeddingParity runs the BGE ONNX model through BGEEmbedder and
// asserts each output cosine-matches the Python reference vector at
// minCosine or better. Skips cleanly if any artifact is missing.
func TestEmbeddingParity(t *testing.T) {
	vocabPath := testVocabPath()
	libPath := testLibPath()
	modelPath := testModelPath()
	if vocabPath == "" || libPath == "" || modelPath == "" {
		t.Skip("GUILD_EMBED_TEST_VOCAB / _LIB / _MODEL not all set; skipping embedding parity")
	}

	emb, err := NewBGEEmbedder(RuntimeConfig{
		LibraryPath: libPath,
		ModelPath:   modelPath,
		VocabPath:   vocabPath,
		NumThreads:  1,
	})
	if err != nil {
		t.Fatalf("NewBGEEmbedder: %v", err)
	}
	defer emb.Close()

	ref := loadReferenceVectors(t)
	ctx := context.Background()
	worst := 1.0
	for text, want := range ref {
		got, err := emb.Embed(ctx, text)
		if err != nil {
			t.Fatalf("Embed(%q): %v", text, err)
		}
		if len(got) != Dim {
			t.Errorf("Embed(%q) returned %d-dim vector, want %d", text, len(got), Dim)
			continue
		}
		sim := cosine(got, want.Embedding)
		if sim < worst {
			worst = sim
		}
		if sim < minCosine {
			t.Errorf("cosine parity for %q: %.6f (want >= %.6f)", text, sim, minCosine)
		} else {
			t.Logf("parity %q: cosine=%.6f", text, sim)
		}
	}
	t.Logf("worst cosine across %d reference entries: %.6f (floor %.6f)", len(ref), worst, minCosine)
}

// TestEmbedderInterface is a tiny smoke test that exercises both
// always-present embedders end-to-end and asserts the interface
// contracts. Does not require any ORT artifacts.
func TestEmbedderInterface(t *testing.T) {
	ctx := context.Background()

	null := NewNullEmbedder()
	if null.Dimension() != Dim {
		t.Errorf("NullEmbedder.Dimension() = %d, want %d", null.Dimension(), Dim)
	}
	if v, err := null.Embed(ctx, "hello"); err == nil {
		t.Errorf("NullEmbedder.Embed returned %d-dim vector, want ErrEmbedderDisabled", len(v))
	}

	det := NewDeterministicEmbedder()
	if det.Dimension() != Dim {
		t.Errorf("DeterministicEmbedder.Dimension() = %d, want %d", det.Dimension(), Dim)
	}
	v1, err := det.Embed(ctx, "hello")
	if err != nil {
		t.Fatalf("DeterministicEmbedder.Embed: %v", err)
	}
	if len(v1) != Dim {
		t.Errorf("DeterministicEmbedder.Embed returned %d-dim vector, want %d", len(v1), Dim)
	}
	// Same input should give the same vector.
	v2, err := det.Embed(ctx, "hello")
	if err != nil {
		t.Fatalf("DeterministicEmbedder.Embed (2nd call): %v", err)
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Errorf("DeterministicEmbedder non-deterministic at index %d: %f != %f", i, v1[i], v2[i])
			break
		}
	}
	// Different input should give different vector.
	v3, err := det.Embed(ctx, "world")
	if err != nil {
		t.Fatalf("DeterministicEmbedder.Embed (3rd): %v", err)
	}
	same := true
	for i := range v1 {
		if v1[i] != v3[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("DeterministicEmbedder returned same vector for different inputs")
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
