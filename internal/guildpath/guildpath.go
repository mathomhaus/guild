// Package guildpath centralises filesystem locations for guild's local
// state and enforces the permissions invariants that #79 calls out:
//
//   - ~/.guild/ is created with 0o700 (private to the user). The
//     directory is touched on every MCP open and every CLI verb, so a
//     stale 0o755 (default umask) from any one of those call sites
//     would re-loosen the dir on a fresh install. EnsureGuildDir
//     centralises that decision so every entry point routes through
//     the same MkdirAll.
//
//   - Every SQLite DB plus its journal_mode=WAL sidecars (-wal, -shm,
//     -journal) is 0o600. modernc/sqlite respects O_CREAT perms on
//     the main file but does not tighten existing files, and the
//     sidecars are always created at the default umask (typically
//     0o644). TightenDBPerms walks all four paths after storage.Open
//     so existing-install corpora are locked down too.
package guildpath

import (
	"fmt"
	"os"
	"path/filepath"
)

// DirPerm is the mode bits applied to ~/.guild/.
const DirPerm os.FileMode = 0o700

// DBPerm is the mode bits applied to a SQLite db file and its WAL/SHM/journal sidecars.
const DBPerm os.FileMode = 0o600

// EnsureGuildDir returns the path to ~/.guild, creating it with 0o700
// if absent. Idempotent: an existing directory keeps its current mode
// (we do not retroactively chmod, since the user may have set 0o750
// deliberately for an audit group). Callers that need the path
// hardened on a stale install should follow up with TightenDir.
func EnsureGuildDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("guildpath: resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return "", fmt.Errorf("guildpath: create %s: %w", dir, err)
	}
	return dir, nil
}

// EnsureDir is the generic variant for sub-paths that derive from
// EnsureGuildDir. It MkdirAlls dir with 0o700, then returns it.
func EnsureDir(dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return fmt.Errorf("guildpath: create %s: %w", dir, err)
	}
	return nil
}

// TightenDBPerms chmods the SQLite main file at path and its
// journal_mode=WAL sidecars (-wal, -shm, -journal) to 0o600. Missing
// siblings are skipped silently, so it is safe to call before the WAL
// is materialised; the only error returned is a real chmod failure on
// a file that exists. Call after storage.Open.
func TightenDBPerms(path string) error {
	if path == "" || path == ":memory:" {
		return nil
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		p := path + suffix
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("guildpath: stat %s: %w", p, err)
		}
		if err := os.Chmod(p, DBPerm); err != nil {
			return fmt.Errorf("guildpath: chmod %s: %w", p, err)
		}
	}
	return nil
}
