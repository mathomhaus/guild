package guildpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureGuildDir_CreatesWith0700(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := EnsureGuildDir()
	if err != nil {
		t.Fatalf("EnsureGuildDir: %v", err)
	}
	if dir != filepath.Join(home, ".guild") {
		t.Errorf("dir = %q; want %q", dir, filepath.Join(home, ".guild"))
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if got := info.Mode().Perm(); got != DirPerm {
		t.Errorf("perm = %o; want %o", got, DirPerm)
	}
}

func TestEnsureGuildDir_Idempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	d1, err := EnsureGuildDir()
	if err != nil {
		t.Fatalf("first EnsureGuildDir: %v", err)
	}
	d2, err := EnsureGuildDir()
	if err != nil {
		t.Fatalf("second EnsureGuildDir: %v", err)
	}
	if d1 != d2 {
		t.Errorf("paths differ: %q vs %q", d1, d2)
	}
}

// Regression: when a stale 0o755 dir pre-exists (e.g. created by a
// prior CLI verb before this fix), EnsureGuildDir must not chmod it
// back to 0o700. We document and rely on the umask-preserving
// idempotence; sites that need a guaranteed-tight dir should call
// os.Chmod after EnsureGuildDir, but we do not foreclose other
// callers' deliberate widening.
func TestEnsureGuildDir_PreservesExistingMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("pre-seed dir: %v", err)
	}
	if _, err := EnsureGuildDir(); err != nil {
		t.Fatalf("EnsureGuildDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("perm = %o; EnsureGuildDir must not retroactively chmod (#79 comment)", got)
	}
}

func TestTightenDBPerms_AllSiblings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lore.db")
	// Seed all four siblings with the default 0o644 sqlite ships them at.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		p := path + suffix
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { //nolint:gosec // test fixture mirrors sqlite's default
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	if err := TightenDBPerms(path); err != nil {
		t.Fatalf("TightenDBPerms: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		p := path + suffix
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != DBPerm {
			t.Errorf("%s perm = %o; want %o", p, got, DBPerm)
		}
	}
}

func TestTightenDBPerms_MissingSiblings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quest.db")
	// Only the main file exists; sidecars haven't been materialised yet.
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("seed: %v", err)
	}
	if err := TightenDBPerms(path); err != nil {
		t.Fatalf("TightenDBPerms: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != DBPerm {
		t.Errorf("main file perm = %o; want %o", got, DBPerm)
	}
}

func TestTightenDBPerms_MemoryPath(t *testing.T) {
	if err := TightenDBPerms(":memory:"); err != nil {
		t.Errorf("TightenDBPerms(:memory:) should be a no-op, got %v", err)
	}
	if err := TightenDBPerms(""); err != nil {
		t.Errorf("TightenDBPerms(\"\") should be a no-op, got %v", err)
	}
}

// Regression for the install-path race: a CLI verb that ran before
// `guild init` could create ~/.guild at 0o755 and the install would
// then no-op. Route the install path through EnsureGuildDir so the
// first-creator semantics are consistent.
func TestEnsureGuildDir_FirstCreatorWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := EnsureGuildDir()
	if err != nil {
		t.Fatalf("EnsureGuildDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != DirPerm {
		t.Errorf("first-creator perm = %o; want %o", got, DirPerm)
	}
}
