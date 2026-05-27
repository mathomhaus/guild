package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/storage"
)

// dbLayout captures the canonical ~/.guild/* storage paths. Split into
// package-level vars (not constants) so tests can swap to t.TempDir.
// Matches the CLI's openLoreDB/openQuestDB layout in internal/cli but
// lives here so internal/mcp doesn't import internal/cli.
var (
	// loreDBPath is the canonical ~/.guild/lore.db location.
	// Tests override via ldbPath func seam below.
	ldbPath = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("mcp: resolve home: %w", err)
		}
		return filepath.Join(home, ".guild", "lore.db"), nil
	}

	// qdbPath is the canonical ~/.guild/quest.db location.
	qdbPath = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("mcp: resolve home: %w", err)
		}
		return filepath.Join(home, ".guild", "quest.db"), nil
	}
)

// openLoreDB opens + migrates lore.db. The MCP server re-opens its
// handle per-tool-call: every tool is a short-lived RPC, and keeping a
// long-lived handle would serialize all tools through one goroutine.
// SQLite's WAL mode (see internal/storage) makes concurrent opens cheap.
func openLoreDB(ctx context.Context) (*sql.DB, error) {
	path, err := ldbPath()
	if err != nil {
		return nil, err
	}
	return openDB(ctx, path, "lore")
}

// openQuestDB opens + migrates quest.db. Same reasoning as openLoreDB.
func openQuestDB(ctx context.Context) (*sql.DB, error) {
	path, err := qdbPath()
	if err != nil {
		return nil, err
	}
	return openDB(ctx, path, "quest")
}

// openDB is the shared open+migrate flow. Extracted so lore and quest
// share the same MkdirAll + migrate plumbing without drift.
func openDB(ctx context.Context, path, description string) (*sql.DB, error) {
	if path != ":memory:" && !strings.HasPrefix(path, ":memory:") {
		if dir := filepath.Dir(path); dir != "." && dir != "/" {
			if err := guildpath.EnsureDir(dir); err != nil {
				return nil, fmt.Errorf("mcp: %w", err)
			}
		}
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := guildpath.TightenDBPerms(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mcp: %w", err)
	}
	if err := storage.Migrate(ctx, db, description); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
