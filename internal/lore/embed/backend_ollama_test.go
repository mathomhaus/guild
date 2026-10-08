package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOllamaBackend_RegisteredAndSelectable proves the Ollama HTTP backend
// self-registered and is reachable through BuildEmbedder by name.
func TestOllamaBackend_RegisteredAndSelectable(t *testing.T) {
	if !HasEmbedder(OllamaBackend) {
		t.Fatalf("expected %q backend registered by init()", OllamaBackend)
	}
	// A model name is required for Ollama.
	if _, err := BuildEmbedder(EmbedConfig{Backend: OllamaBackend}); err == nil {
		t.Error("ollama backend without a model must error")
	}
	emb, err := BuildEmbedder(EmbedConfig{Backend: OllamaBackend, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("BuildEmbedder(ollama): %v", err)
	}
	if _, ok := emb.(*ollamaEmbedder); !ok {
		t.Fatalf("BuildEmbedder returned %T, want *ollamaEmbedder", emb)
	}
	if emb.Dimension() != Dim {
		t.Errorf("ollama Dimension = %d, want %d", emb.Dimension(), Dim)
	}
}

// TestOllamaBackend_EmbedViaHTTPTest exercises a full Embed round trip against
// an httptest server, so the real HTTP backend is proven end to end with NO
// live network.
func TestOllamaBackend_EmbedViaHTTPTest(t *testing.T) {
	var gotModel, gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req ollamaEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotModel, gotPrompt = req.Model, req.Prompt
		vec := make([]float32, Dim)
		vec[0] = 0.5
		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{Embedding: vec})
	}))
	defer srv.Close()

	t.Setenv(OllamaBaseURLEnv, srv.URL)
	emb, err := BuildEmbedder(EmbedConfig{Backend: OllamaBackend, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("BuildEmbedder: %v", err)
	}
	vec, err := emb.Embed(context.Background(), "the cat sat on the mat")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != Dim {
		t.Fatalf("vector len = %d, want %d", len(vec), Dim)
	}
	if vec[0] != 0.5 {
		t.Errorf("vec[0] = %v, want 0.5", vec[0])
	}
	if gotModel != "nomic-embed-text" {
		t.Errorf("server saw model %q, want nomic-embed-text", gotModel)
	}
	if gotPrompt != "the cat sat on the mat" {
		t.Errorf("server saw prompt %q", gotPrompt)
	}
}

// TestOllamaBackend_DimMismatchErrors proves a wrong-width response is a loud
// error, never a silently-truncated vector that would poison cosine fusion.
func TestOllamaBackend_DimMismatchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{Embedding: []float32{1, 2, 3}}) // wrong width
	}))
	defer srv.Close()
	t.Setenv(OllamaBaseURLEnv, srv.URL)
	emb, err := BuildEmbedder(EmbedConfig{Backend: OllamaBackend, Model: "m"})
	if err != nil {
		t.Fatalf("BuildEmbedder: %v", err)
	}
	if _, err := emb.Embed(context.Background(), "x"); err == nil {
		t.Error("a wrong-dimension response must error")
	}
}

// TestOllamaBackend_HTTPErrorPropagates proves a non-200 surfaces as an error.
func TestOllamaBackend_HTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv(OllamaBaseURLEnv, srv.URL)
	emb, err := BuildEmbedder(EmbedConfig{Backend: OllamaBackend, Model: "m"})
	if err != nil {
		t.Fatalf("BuildEmbedder: %v", err)
	}
	if _, err := emb.Embed(context.Background(), "x"); err == nil {
		t.Error("a 500 response must error")
	}
}
