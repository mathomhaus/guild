//go:build unix

package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/daemon"
)

// setTestHome points $HOME at a fresh temp dir so the probe reads a
// hermetic ~/.guild and never the live one.
func setTestHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// disableAutostart sets GUILD_NO_DAEMON so a probe-miss takes the
// byte-identical no-daemon path instead of spawning a daemon. Used by
// the tests that assert the opt-out behavior (silent, instant
// fall-through); the autostart-on behavior is covered separately in
// the daemon package.
func disableAutostart(t *testing.T) {
	t.Helper()
	t.Setenv("GUILD_NO_DAEMON", "1")
}

// testSocket returns a listening unix socket in a short-named temp dir
// (sun_path is capped near 104 bytes on darwin, and t.TempDir() paths
// can exceed it).
func testSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	if len(sock) > 100 {
		t.Skipf("temp socket path too long for sun_path: %s", sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return sock
}

// TestShimProbeBudget_NoDaemon pins the two probe-budget guarantees on
// the autostart-disabled (opt-out) path: the configured dial timeout
// stays within the 250ms ceiling, and with no daemon at all (no
// discovery file, so not even a dial) the whole decision is effectively
// instant, with not a byte written anywhere.
func TestShimProbeBudget_NoDaemon(t *testing.T) {
	setTestHome(t)
	disableAutostart(t)

	if shimProbeTimeout > 250*time.Millisecond {
		t.Fatalf("shimProbeTimeout = %v; must not exceed 250ms", shimProbeTimeout)
	}

	var out bytes.Buffer
	start := time.Now()
	done, err := tryDaemonShim(context.Background(), "v-self", &out)
	elapsed := time.Since(start)

	if done || err != nil {
		t.Fatalf("tryDaemonShim with no daemon = (done=%v, err=%v); want (false, nil) so mcp.Serve runs as today", done, err)
	}
	if out.Len() != 0 {
		t.Fatalf("no-daemon path produced output %q; must be silent", out.String())
	}
	if elapsed > shimProbeTimeout {
		t.Fatalf("no-daemon probe took %v; budget is %v", elapsed, shimProbeTimeout)
	}
}

// TestTryDaemonShim_VersionMismatch_OneNudgeLine: a live daemon on a
// different version yields exactly one stderr nudge naming both
// versions and suggesting a daemon restart, then the in-process path.
func TestTryDaemonShim_VersionMismatch_OneNudgeLine(t *testing.T) {
	setTestHome(t)
	sock := testSocket(t)

	if err := daemon.WriteDiscovery(daemon.Discovery{
		PID:        os.Getpid(), // alive, so the probe reaches the version check
		Version:    "v9.9.9",
		SocketPath: sock,
		StartedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}

	var out bytes.Buffer
	done, err := tryDaemonShim(context.Background(), "v1.0.0", &out)
	if done || err != nil {
		t.Fatalf("tryDaemonShim on mismatch = (done=%v, err=%v); want (false, nil) so mcp.Serve runs in-process", done, err)
	}

	nudge := out.String()
	if got := strings.Count(nudge, "\n"); got != 1 {
		t.Fatalf("want exactly one nudge line; got %d in %q", got, nudge)
	}
	for _, want := range []string{"v9.9.9", "v1.0.0", "estart the daemon"} {
		if !strings.Contains(nudge, want) {
			t.Fatalf("nudge %q missing %q", nudge, want)
		}
	}
}

// TestTryDaemonShim_StaleDiscovery_SilentInProcess: a discovery record
// whose socket no longer dials is "not running", which must stay
// completely silent like the no-daemon case.
func TestTryDaemonShim_StaleDiscovery_SilentInProcess(t *testing.T) {
	setTestHome(t)
	disableAutostart(t)

	if err := daemon.WriteDiscovery(daemon.Discovery{
		PID:        os.Getpid(),
		Version:    "v-self",
		SocketPath: filepath.Join(t.TempDir(), "gone.sock"),
		StartedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}

	var out bytes.Buffer
	done, err := tryDaemonShim(context.Background(), "v-self", &out)
	if done || err != nil {
		t.Fatalf("tryDaemonShim on stale discovery = (done=%v, err=%v); want (false, nil)", done, err)
	}
	if out.Len() != 0 {
		t.Fatalf("stale-discovery path produced output %q; must be silent", out.String())
	}
}

// TestTryDaemonShim_OptOut_NoSideEffects pins the ADR-005 hard
// invariant: with autostart disabled (GUILD_NO_DAEMON=1), a probe-miss
// goes straight in-process with zero new side effects: no lock file
// created, no spawn attempt, no output, and the decision stays instant.
func TestTryDaemonShim_OptOut_NoSideEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	disableAutostart(t)

	var out bytes.Buffer
	start := time.Now()
	done, err := tryDaemonShim(context.Background(), "v-self", &out)
	elapsed := time.Since(start)

	if done || err != nil {
		t.Fatalf("opt-out probe-miss = (done=%v, err=%v); want (false, nil)", done, err)
	}
	if out.Len() != 0 {
		t.Fatalf("opt-out path produced output %q; must be silent", out.String())
	}
	if elapsed > shimProbeTimeout {
		t.Fatalf("opt-out probe took %v; budget is %v (no spawn or wait should happen)", elapsed, shimProbeTimeout)
	}
	// No lock file may exist: the opt-out path must never reach the
	// election machinery. (~/.guild may not even have been created.)
	lock := filepath.Join(home, ".guild", "daemon.lock")
	if _, statErr := os.Stat(lock); statErr == nil {
		t.Fatalf("opt-out path created %s; the no-daemon path must touch no lock file", lock)
	}
}
