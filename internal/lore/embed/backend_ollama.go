package embed

// Ollama HTTP embedder backend (ADR-006 Phase 4, deliverable 4).
//
// A production-registered ALTERNATE backend behind the same Embedder interface,
// proving the registry carries a real (network-capable) backend, not only an
// in-process stub. It speaks Ollama's local embeddings API
// (POST {base}/api/embeddings -> {"embedding": [...]}), the local-first choice
// that fits guild's no-cloud-dependency posture: a user running ollama locally
// can set [embed].backend = "ollama" and select it.
//
// NO live network is taken in tests: the base URL is overridable via the
// GUILD_OLLAMA_BASE_URL env var so a unit test points it at httptest.Server,
// and the backend is only constructed when config names it. The default path
// never touches this code.
//
// This is deliberately small: one POST, JSON in/out, a Dim-length guard. A
// richer client (batching, retries, auth) is out of scope for the Phase-4 seam;
// the point is that the registry boundary carries an HTTP backend cleanly.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// OllamaBackend is the registry name for the Ollama HTTP embedder.
const OllamaBackend = "ollama"

// ollamaDefaultBaseURL is the conventional local Ollama endpoint. Overridable
// via GUILD_OLLAMA_BASE_URL (the test seam and the operator escape hatch).
const ollamaDefaultBaseURL = "http://127.0.0.1:11434"

// OllamaBaseURLEnv overrides the Ollama endpoint. Documented so operators can
// point at a non-default host and tests can point at httptest.
const OllamaBaseURLEnv = "GUILD_OLLAMA_BASE_URL"

func init() {
	RegisterEmbedder(OllamaBackend, newOllamaEmbedder)
}

// newOllamaEmbedder constructs the Ollama HTTP embedder. cfg.Model selects the
// Ollama embedding model (e.g. "nomic-embed-text"); empty is rejected because
// Ollama requires a model name. The base URL comes from GUILD_OLLAMA_BASE_URL
// or the local default. No connection is made at construction; the first Embed
// call dials.
func newOllamaEmbedder(cfg EmbedConfig) (Embedder, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("embed: ollama backend requires [embed].model (the Ollama embedding model name)")
	}
	base := os.Getenv(OllamaBaseURLEnv)
	if base == "" {
		base = ollamaDefaultBaseURL
	}
	return &ollamaEmbedder{
		baseURL: base,
		model:   cfg.Model,
		client:  &http.Client{Timeout: 30 * time.Second},
		dim:     Dim,
	}, nil
}

// ollamaEmbedder calls Ollama's embeddings endpoint over HTTP. Safe for
// concurrent use: http.Client is concurrency-safe and the struct is read-only
// after construction.
type ollamaEmbedder struct {
	baseURL string
	model   string
	client  *http.Client
	dim     int
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed POSTs text to {base}/api/embeddings and returns the embedding vector.
// It enforces the package Dim contract so a mismatched model surfaces as a
// loud error rather than polluting cosine fusion with a wrong-length vector.
func (e *ollamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(ollamaEmbedRequest{Model: e.model, Prompt: text})
	if err != nil {
		return nil, fmt.Errorf("embed: ollama marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: ollama new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: ollama request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: ollama status %d", resp.StatusCode)
	}

	var out ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: ollama decode: %w", err)
	}
	if len(out.Embedding) != e.dim {
		return nil, fmt.Errorf("embed: ollama returned %d dims, want %d (model %q mismatch?): %w",
			len(out.Embedding), e.dim, e.model, ErrUnexpectedOutputShape)
	}
	return out.Embedding, nil
}

// Dimension returns the package Dim. The Ollama model must emit this width;
// Embed enforces it per call.
func (e *ollamaEmbedder) Dimension() int {
	if e.dim <= 0 {
		return Dim
	}
	return e.dim
}

var _ Embedder = (*ollamaEmbedder)(nil)
