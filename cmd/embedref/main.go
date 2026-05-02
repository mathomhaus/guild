// Command embedref regenerates internal/lore/embed/testdata/reference_vectors.json
// against the BUNDLED int8 model + tokenizer + libonnxruntime, so the
// checked-in fixture reflects what the shipping binary actually produces.
//
// Why this tool exists: QUEST-207 committed reference vectors generated
// against the fp32 model (cosine=1.0 against fp32), but the binary ships
// the int8 quantized model. int8 quantization drops cosine to ~0.98 vs
// fp32, which silently failed the 0.999 probe floor on a clean install
// (QUEST-215). The authoritative reference MUST match the bundled model;
// this tool re-anchors it.
//
// Usage:
//
//	go run -tags=withembed ./cmd/embedref > internal/lore/embed/testdata/reference_vectors.json
//
// Or via the Makefile:
//
//	make regenerate-reference-vectors
//
// Must be built with -tags=withembed so the bundled manifest carries the
// asset bytes. On unsupported triples (no bundled assets) the tool exits
// non-zero with a clear message.

//go:build withembed && unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// referenceInputs is the pinned list of probe + parity strings.
//
// The first entry (embed.ProbeString) MUST be present because the probe
// fixture path reads that key at init. The remaining four stay matched
// with the spike's original set so parity coverage is unchanged.
var referenceInputs = []string{
	"retry logic with exponential backoff", // embed.ProbeString
	"SQLITE_BUSY error under contention",
	"authentication token expiry",
	"guild init skips already-registered clients",
	"BM25 FTS5 lexical retrieval",
}

type refEntry struct {
	InputIDs      []int64   `json:"input_ids"`
	AttentionMask []int64   `json:"attention_mask"`
	TokenTypeIDs  []int64   `json:"token_type_ids"`
	Embedding     []float32 `json:"embedding"`
}

type provenance struct {
	GeneratedAt    string `json:"generated_at"`
	ModelID        string `json:"model_id"`
	TokenizerHash  string `json:"tokenizer_hash"`
	RuntimeVersion string `json:"runtime_version"`
	PlatformTag    string `json:"platform_tag"`
	LibrarySHA256  string `json:"library_sha256"`
	ModelSHA256    string `json:"model_sha256"`
	VocabSHA256    string `json:"vocab_sha256"`
	Note           string `json:"note"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embedref:", err)
		os.Exit(1)
	}
}

func run() error {
	man := embed.CurrentManifest()
	if !embed.HasAssets() {
		return fmt.Errorf("no bundled assets in this build; build with -tags=withembed on a supported triple")
	}
	if len(man.Assets) < 3 {
		return fmt.Errorf("bundled manifest has %d assets, expected 3", len(man.Assets))
	}

	tmp, err := os.MkdirTemp("", "embedref-")
	if err != nil {
		return fmt.Errorf("mkdir temp: %w", err)
	}
	defer os.RemoveAll(tmp)

	cacheDir := filepath.Join(tmp, man.Identity.PlatformTag)
	ext, err := embed.Extract(man, cacheDir)
	if err != nil {
		return fmt.Errorf("extract bundled assets: %w", err)
	}

	emb, err := embed.NewBGEEmbedder(embed.RuntimeConfig{
		LibraryPath: ext.LibraryPath,
		ModelPath:   ext.ModelPath,
		VocabPath:   ext.VocabPath,
		NumThreads:  1, // determinism
	})
	if err != nil {
		return fmt.Errorf("new BGE embedder: %w", err)
	}
	defer emb.Close()

	// Tokenizer for capturing input_ids/attention_mask/token_type_ids.
	// Loaded from the bundled vocab so the fixture's ids match the
	// extracted file exactly.
	vocab, err := embed.LoadVocab(ext.VocabPath)
	if err != nil {
		return fmt.Errorf("load vocab: %w", err)
	}
	tk := embed.NewWordPieceTokenizer(vocab)

	ctx := context.Background()
	entries := make(map[string]refEntry, len(referenceInputs))
	for _, text := range referenceInputs {
		ids, mask, typeIDs := tk.Encode(text, 512)
		vec, err := emb.Embed(ctx, text)
		if err != nil {
			return fmt.Errorf("embed %q: %w", text, err)
		}
		entries[text] = refEntry{
			InputIDs:      ids,
			AttentionMask: mask,
			TokenTypeIDs:  typeIDs,
			Embedding:     vec,
		}
	}

	// Emit provenance on stderr so stdout stays a pristine JSON document
	// suitable for redirection into testdata/.
	prov := provenance{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		ModelID:        man.Identity.ModelID,
		TokenizerHash:  man.Identity.TokenizerHash,
		RuntimeVersion: man.Identity.RuntimeVersion,
		PlatformTag:    man.Identity.PlatformTag,
		LibrarySHA256:  man.Assets[embed.AssetLibrary].SHA256,
		ModelSHA256:    man.Assets[embed.AssetModel].SHA256,
		VocabSHA256:    man.Assets[embed.AssetVocab].SHA256,
		Note:           "Generated against the BUNDLED int8 model. Fixture is the ground truth for RunProbe and TestEmbeddingParity.",
	}
	provJSON, _ := json.MarshalIndent(prov, "", "  ")
	slog.Info("embedref provenance", slog.String("json", string(provJSON)))
	fmt.Fprintln(os.Stderr, string(provJSON))

	// Stable key order for reproducible diffs.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]refEntry, len(entries))
	// json.Marshal emits map keys sorted, so feeding the map is fine for
	// stable output. Keeping the explicit sort for documentation.
	for _, k := range keys {
		ordered[k] = entries[k]
	}
	out, err := json.Marshal(ordered)
	if err != nil {
		return fmt.Errorf("marshal fixture: %w", err)
	}
	// Trailing newline keeps git diff noise to a minimum.
	if _, err := os.Stdout.Write(out); err != nil {
		return err
	}
	if _, err := os.Stdout.Write([]byte("\n")); err != nil {
		return err
	}
	return nil
}
