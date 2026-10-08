// daemon_watch_parity_test.go: ADR-005 Phase 4 parity backstop for the
// watch -> staleness -> renewal pipeline.
//
// The hard ADR invariant is that correctness never depends on the daemon
// and the no-daemon path stays byte-identical to today. The watcher is
// the first daemon feature that, when ON, deliberately writes new rows
// (staleness signals, renewal quests). This file enforces both halves:
//
//   - daemon-DOWN: the identical operations (register a project, seed a
//     current entry citing a tracked file, modify that file) produce ZERO
//     staleness signals and ZERO renewal quests. Nothing watches, so
//     staleness stays a query-time concept exactly as before the daemon.
//   - daemon-UP: the same modification yields, within a bounded deadline,
//     exactly one open renewal quest, and a second modification does not
//     mint a second one (the poster's dedupe). This proves the wiring
//     actually fires while the negative arm proves it stays optional.
//
// The entry's file_path is patched directly in lore.db because the CLI
// inscribe verb does not expose it; everything else (project register,
// entry create, renewal observation) goes through the real binary. Unix
// only, matching the daemon lifecycle and parity suites.
//
//go:build unix

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// watchRenewalReadyTimeout bounds how long the daemon-up arm polls for the
// watcher to flag the entry and post its renewal quest. Generous so a
// loaded runner does not flake; the poll exits the instant the quest
// appears. It must comfortably exceed the watcher's one-second debounce.
const watchRenewalReadyTimeout = 25 * time.Second

// seedTrackedEntry inscribes a never-stale principle entry through the
// real CLI, then patches its file_path to absPath directly in lore.db
// (the CLI has no file_path flag). A principle never auto-stales, so the
// only path to a staleness signal is the watcher seeing absPath change:
// any signal the test observes is unambiguously the watcher's doing.
func seedTrackedEntry(ctx context.Context, t *testing.T, homeDir, projDir, absPath string) {
	t.Helper()

	inv := runArgs(ctx, t, homeDir, projDir, []string{
		"lore", "inscribe", "watch pipeline fixture",
		"--kind", "principle",
		"--no-warn",
		"--summary", "Fixture entry for the watch pipeline parity test.",
		"--topic", "watch-parity",
	})
	if inv.ExitCode != 0 {
		t.Fatalf("lore inscribe: exit %d\nstdout: %s\nstderr: %s", inv.ExitCode, inv.Stdout, inv.Stderr)
	}

	loreDB := filepath.Join(homeDir, ".guild", "lore.db")
	db, err := storage.Open(ctx, loreDB)
	if err != nil {
		t.Fatalf("open lore.db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Store the symlink-resolved path: the watcher reports the canonical
	// path its backend sees (on darwin /var/folders resolves to
	// /private/var/folders), and FlagStaleByPath matches file_path
	// exactly. A real project registers its git-toplevel, which is
	// likewise canonical, so resolving here mirrors production rather than
	// papering over a path mismatch.
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		t.Fatalf("resolve tracked path: %v", err)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE entries SET file_path = ? WHERE status = 'current' AND file_path IS NULL`,
		resolved)
	if err != nil {
		t.Fatalf("patch file_path: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("patched %d entries, want exactly 1", n)
	}
}

// countStalenessSignals returns the staleness_signals row count in lore.db
// under homeDir. Zero is the daemon-down invariant.
func countStalenessSignals(ctx context.Context, t *testing.T, homeDir string) int {
	t.Helper()
	loreDB := filepath.Join(homeDir, ".guild", "lore.db")
	db, err := storage.Open(ctx, loreDB)
	if err != nil {
		t.Fatalf("open lore.db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM staleness_signals`).Scan(&n); err != nil {
		// The table exists from migration 010; a query error is a real
		// failure, not an empty result.
		if err == sql.ErrNoRows {
			return 0
		}
		t.Fatalf("count staleness_signals: %v", err)
	}
	return n
}

// countOpenRenewalQuests parses `quest list --epic lore-renewal --json`
// and returns how many open renewal quests the binary reports. Going
// through the CLI keeps the observation on the same surface an operator
// sees, rather than a second SQL read.
func countOpenRenewalQuests(ctx context.Context, t *testing.T, homeDir, projDir string) int {
	t.Helper()
	inv := runArgsEnv(ctx, t, homeDir, projDir, nil,
		[]string{"quest", "list", "--epic", "lore-renewal", "--json"})
	if inv.ExitCode != 0 {
		t.Fatalf("quest list --epic lore-renewal: exit %d\nstdout: %s\nstderr: %s",
			inv.ExitCode, inv.Stdout, inv.Stderr)
	}
	if inv.Stdout == "" {
		return 0
	}
	// Two JSON shapes reach here. Plain --json (no TTY) emits a top-level
	// {"quests":[...]}; the agent envelope (auto-detected on a TTY) wraps
	// it as {"ok":...,"output":{"quests":[...]}}. An empty result reports
	// "quests":null in either. Parse both so the test is robust to which
	// path produced the bytes.
	var doc struct {
		Quests []json.RawMessage `json:"quests"`
		Output struct {
			Quests []json.RawMessage `json:"quests"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(inv.Stdout), &doc); err != nil {
		t.Fatalf("quest list --json: unparseable output: %s\nerr: %v", inv.Stdout, err)
	}
	if doc.Quests != nil {
		return len(doc.Quests)
	}
	return len(doc.Output.Quests)
}

// TestWatchParity_NoDaemonProducesNothing is the negative backstop: with
// the daemon off, modifying a tracked source file produces zero staleness
// signals and zero renewal quests. Staleness stays query-time only, the
// byte-identical no-daemon behavior the ADR requires.
func TestWatchParity_NoDaemonProducesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watch parity (subprocess) in -short mode")
	}
	ctx := context.Background()

	home := shortHome(t)
	pinAutostartOff(t, home)
	projDir := filepath.Join(home, parityProject)
	_ = initProject(ctx, t, home, projDir)

	tracked := filepath.Join(projDir, "tracked.md")
	if err := os.WriteFile(tracked, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed tracked file: %v", err)
	}
	seedTrackedEntry(ctx, t, home, projDir, tracked)

	// Modify the tracked file: a daemon would react; no daemon must not.
	if err := os.WriteFile(tracked, []byte("v2 changed"), 0o600); err != nil {
		t.Fatalf("modify tracked file: %v", err)
	}
	// Give any (wrongly) spawned watcher more than a debounce window to
	// act, so the assertion is meaningful rather than merely fast.
	time.Sleep(2 * time.Second)

	if got := countStalenessSignals(ctx, t, home); got != 0 {
		t.Errorf("daemon-down: %d staleness signals, want 0 (no watcher must run)", got)
	}
	if got := countOpenRenewalQuests(ctx, t, home, projDir); got != 0 {
		t.Errorf("daemon-down: %d renewal quests, want 0 (no watcher must run)", got)
	}
	assertNoDaemonSideEffects(t, home, projDir)
}

// TestWatchParity_DaemonFlagsAndRenewsOnce is the positive arm: with a
// daemon up and watching, modifying the tracked file yields exactly one
// open renewal quest within the deadline, and a second modification does
// not mint a second open renewal quest (the poster's dedupe).
func TestWatchParity_DaemonFlagsAndRenewsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watch parity (subprocess + daemon) in -short mode")
	}
	ctx := context.Background()

	home := shortHome(t)
	projDir := filepath.Join(home, parityProject)
	_ = initProject(ctx, t, home, projDir)

	tracked := filepath.Join(projDir, "tracked.md")
	if err := os.WriteFile(tracked, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed tracked file: %v", err)
	}
	seedTrackedEntry(ctx, t, home, projDir, tracked)

	// Start the daemon AFTER seeding so its first root enumeration already
	// sees the registered project. The watcher covers projDir.
	startParityDaemon(t, home, projDir)

	if err := os.WriteFile(tracked, []byte("v2 changed"), 0o600); err != nil {
		t.Fatalf("modify tracked file: %v", err)
	}

	waitForRenewalCount(ctx, t, home, projDir, 1)

	// A second modification must not produce a second open renewal quest:
	// an open renewal for the entry already exists, so the poster dedupes.
	if err := os.WriteFile(tracked, []byte("v3 changed again"), 0o600); err != nil {
		t.Fatalf("modify tracked file again: %v", err)
	}
	time.Sleep(3 * time.Second) // more than a debounce + post window
	if got := countOpenRenewalQuests(ctx, t, home, projDir); got != 1 {
		t.Errorf("after second modification: %d open renewal quests, want 1 (dedupe)", got)
	}
}

// waitForRenewalCount polls the renewal quest count until it reaches want
// or the deadline, failing otherwise. The deadline covers the watcher's
// debounce plus the flag+post round trip.
func waitForRenewalCount(ctx context.Context, t *testing.T, homeDir, projDir string, want int) {
	t.Helper()
	deadline := time.Now().Add(watchRenewalReadyTimeout)
	for {
		if got := countOpenRenewalQuests(ctx, t, homeDir, projDir); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("renewal quest count did not reach %d within %s (last=%d)",
				want, watchRenewalReadyTimeout, countOpenRenewalQuests(ctx, t, homeDir, projDir))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
