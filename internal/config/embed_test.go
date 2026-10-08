package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEmbedDefaults asserts the built-in [embed] / [provider] defaults select
// the bundled local embedder and the no-op provider, so a silent config is
// byte-identical to pre-Phase-4 behavior (ADR-006 Phase 4 parity bar).
func TestEmbedDefaults(t *testing.T) {
	d := defaults()
	if d.Embed.Backend != "local-bge" {
		t.Errorf("embed.backend default: got %q want local-bge", d.Embed.Backend)
	}
	if d.Embed.Model != "" {
		t.Errorf("embed.model default: got %q want empty", d.Embed.Model)
	}
	if d.Provider.Backend != "noop" {
		t.Errorf("provider.backend default: got %q want noop", d.Provider.Backend)
	}
}

// TestFileLayerEmbedSection covers a full [embed] / [provider] override and
// the per-key partial-merge contract (a key absent from the file keeps the
// lower-layer value).
func TestFileLayerEmbedSection(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := `[embed]
backend = "openai"
model = "text-embedding-3-small"

[provider]
backend = "anthropic"
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Embed.Backend != "openai" {
		t.Errorf("embed.backend: got %q want openai", cfg.Embed.Backend)
	}
	if cfg.Embed.Model != "text-embedding-3-small" {
		t.Errorf("embed.model: got %q want text-embedding-3-small", cfg.Embed.Model)
	}
	if cfg.Provider.Backend != "anthropic" {
		t.Errorf("provider.backend: got %q want anthropic", cfg.Provider.Backend)
	}
	// provider.model absent from the file: must keep the default (empty).
	if cfg.Provider.Model != "" {
		t.Errorf("provider.model should stay default empty, got %q", cfg.Provider.Model)
	}
}

// TestFileLayerEmbedPartialPreservesLowerLayer proves the per-key merge:
// a file that sets only embed.model must not blank a lower-layer backend.
func TestFileLayerEmbedPartialPreservesLowerLayer(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[embed]\nmodel = \"only-model\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.Embed.Backend = "ollama" // lower layer
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Embed.Backend != "ollama" {
		t.Errorf("embed.backend from lower layer must be preserved, got %q", cfg.Embed.Backend)
	}
	if cfg.Embed.Model != "only-model" {
		t.Errorf("embed.model: got %q want only-model", cfg.Embed.Model)
	}
}

// TestEnvEmbedBackendOverride proves GUILD_EMBED_BACKEND / GUILD_PROVIDER_BACKEND
// override the file layer (env precedence) and an empty value is a no-op.
func TestEnvEmbedBackendOverride(t *testing.T) {
	t.Setenv("GUILD_EMBED_BACKEND", "ollama")
	t.Setenv("GUILD_PROVIDER_BACKEND", "openai")
	cfg := defaults()
	envLayer(&cfg)
	if cfg.Embed.Backend != "ollama" {
		t.Errorf("GUILD_EMBED_BACKEND: got %q want ollama", cfg.Embed.Backend)
	}
	if cfg.Provider.Backend != "openai" {
		t.Errorf("GUILD_PROVIDER_BACKEND: got %q want openai", cfg.Provider.Backend)
	}
}

func TestEnvEmbedBackendEmptyIsNoop(t *testing.T) {
	t.Setenv("GUILD_EMBED_BACKEND", "")
	cfg := defaults()
	envLayer(&cfg)
	if cfg.Embed.Backend != "local-bge" {
		t.Errorf("empty GUILD_EMBED_BACKEND must not blank the default, got %q", cfg.Embed.Backend)
	}
}

// TestLoadEmbedDefaultSilentConfig proves an end-to-end Load with no [embed]
// section yields the default local-bge backend: the byte-identical default.
func TestLoadEmbedDefaultSilentConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embed.Backend != "local-bge" {
		t.Errorf("silent config embed.backend: got %q want local-bge", cfg.Embed.Backend)
	}
	if cfg.Provider.Backend != "noop" {
		t.Errorf("silent config provider.backend: got %q want noop", cfg.Provider.Backend)
	}
}
