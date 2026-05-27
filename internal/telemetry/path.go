// Package telemetry implements privacy-preserving usage logging for guild.
//
// Two log files live under ~/.guild/:
//   - usage.log   — one TSV record per guild command invocation
//   - misses.log  — one TSV record per zero-result lore appraise query
//
// Privacy contract:
//   - usage.log records ONLY: timestamp, project basename, subcommand name,
//     exit code, duration_ms.  No query text, no titles, no summaries,
//     no file paths, no agent IDs, no user content of any kind.
//   - misses.log records project + the verbatim query string.  Query text IS
//     intentionally logged here because it is the retrieval system's input,
//     not user-authored content; this is what makes the misses log useful for
//     corpus improvement.
//
// Opt-in:
//   - [telemetry] usage_log = true in ~/.guild/config.toml
//     (GUILD_NO_USAGE_LOG=1 also forces logging off regardless of config)
package telemetry

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mathomhaus/guild/internal/guildpath"
)

// guildDir returns the path to ~/.guild/, using os.UserHomeDir() so that
// tests can redirect it via the HOME environment variable.
func guildDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("telemetry: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".guild"), nil
}

// UsageLogPath returns the absolute path to ~/.guild/usage.log.
func UsageLogPath() (string, error) {
	dir, err := guildDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "usage.log"), nil
}

// MissesLogPath returns the absolute path to ~/.guild/misses.log.
func MissesLogPath() (string, error) {
	dir, err := guildDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "misses.log"), nil
}

// ensureDir creates dir (and parents) with the canonical ~/.guild
// mode (0o700) if it does not already exist. Routes through
// guildpath so every entry point uses the same first-creator
// semantics (#79).
func ensureDir(dir string) error {
	if err := guildpath.EnsureDir(dir); err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	return nil
}
