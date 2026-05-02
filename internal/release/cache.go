package release

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// CacheEntry is the JSON shape written to ~/.guild/release_check.json.
type CacheEntry struct {
	// CheckedAt is the time the last successful API call was made.
	CheckedAt time.Time `json:"checked_at"`
	// Latest is the latest known release tag, e.g. "v0.3.0".
	Latest string `json:"latest"`
	// URL is the release page HTML URL.
	URL string `json:"url"`
}

// cacheFileName is the filename under ~/.guild/ used to store the cache.
const cacheFileName = "release_check.json"

// cachePath returns the absolute path to the cache file, using os.UserHomeDir.
// Returns an error if the home directory cannot be resolved.
func cachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("release cache: home dir: %w", err)
	}
	return filepath.Join(home, ".guild", cacheFileName), nil
}

// ReadCache reads and parses the on-disk cache. Returns an error when the
// file is missing, unreadable, or contains malformed JSON. Callers should
// treat any error as "cache stale, refetch."
func ReadCache() (CacheEntry, error) {
	path, err := cachePath()
	if err != nil {
		return CacheEntry{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return CacheEntry{}, fmt.Errorf("release cache: read: %w", err)
	}

	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return CacheEntry{}, fmt.Errorf("release cache: parse: %w", err)
	}
	if entry.CheckedAt.IsZero() || entry.Latest == "" {
		return CacheEntry{}, fmt.Errorf("release cache: incomplete entry")
	}
	return entry, nil
}

// WriteCache persists a CacheEntry to ~/.guild/release_check.json. Torn
// or permission-error writes are logged at debug and silently ignored by
// callers. The ~/.guild directory is created if absent.
func WriteCache(entry CacheEntry) error {
	path, err := cachePath()
	if err != nil {
		slog.Debug("release cache: cannot resolve path", "err", err)
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Debug("release cache: mkdir failed", "err", err)
		return fmt.Errorf("release cache: mkdir: %w", err)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("release cache: marshal: %w", err)
	}

	// Write to a temp file then rename for atomic replacement.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Debug("release cache: write tmp failed", "err", err)
		return fmt.Errorf("release cache: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		slog.Debug("release cache: rename failed", "err", err)
		return fmt.Errorf("release cache: rename: %w", err)
	}
	return nil
}
