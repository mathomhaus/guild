package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/storage"
)

// TestBuildMCPLoreDeps_EmbedField confirms the MCP-side Deps builder
// threads currentEmbedProvider into command.Deps.Embed. The test drives
// two shapes: (1) a fresh DB whose meta is schema-seeded to
// "embedder_state=disabled" so the provider resolves to nil; (2) no
// provider installed, demonstrating buildMCPLoreDeps tolerates a nil
// provider without a typed-nil interface hazard.
//
// Default-build machines can't probe the real BGE path without
// -tags=withembed bundled bytes, so the "enabled wiring" assertion is
// covered by TestEmbedProvider_StateFlip below (which uses meta-only
// stubs) and by the internal/lore/embed_wiring_test.go truth table.
func TestBuildMCPLoreDeps_EmbedField(t *testing.T) {
	// Route lore.db through a temp file so the provider does not hit
	// the user's real ~/.guild/lore.db.
	tmpDir := t.TempDir()
	origLdb := ldbPath
	ldbPath = func() (string, error) {
		return filepath.Join(tmpDir, "lore.db"), nil
	}
	t.Cleanup(func() { ldbPath = origLdb })

	// Bring a fresh lore.db into existence with default meta seeds
	// (embedder_state='disabled').
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(tmpDir, "lore.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("storage.Migrate: %v", err)
	}
	_ = db.Close()

	t.Run("disabled_meta_yields_nil_embed_resolve", func(t *testing.T) {
		currentEmbedProvider = newEmbedProvider(openLoreDB, newLogger())
		t.Cleanup(func() { currentEmbedProvider = nil })

		deps := buildMCPLoreDeps()
		if deps.Embed == nil {
			t.Fatalf("buildMCPLoreDeps().Embed should carry the provider, got nil")
		}
		// Resolve through the same path production lore handlers
		// exercise.
		resolver, ok := deps.Embed.(interface {
			ResolveEmbedDeps(ctx context.Context) *lore.EmbedDeps
		})
		if !ok {
			t.Fatalf("deps.Embed has type %T, want embedResolver", deps.Embed)
		}
		got := resolver.ResolveEmbedDeps(ctx)
		if got != nil {
			t.Errorf("ResolveEmbedDeps on disabled meta should return nil; got %+v", got)
		}
	})

	t.Run("nil_provider_leaves_embed_field_nil", func(t *testing.T) {
		currentEmbedProvider = nil
		deps := buildMCPLoreDeps()
		if deps.Embed != nil {
			t.Errorf("buildMCPLoreDeps().Embed = %+v, want nil when provider is nil", deps.Embed)
		}
	})
}
