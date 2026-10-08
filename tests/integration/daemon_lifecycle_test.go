//go:build unix

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// shortHome returns a short-named temp HOME for daemon tests. The
// daemon's canonical unix socket lives at $HOME/.guild/daemon.sock,
// and t.TempDir() paths embed the full test name, which can blow past
// the sun_path cap (104 bytes on darwin); os.MkdirTemp keeps the
// prefix minimal.
func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if p := filepath.Join(home, ".guild", "daemon.sock"); len(p) > 100 {
		t.Skipf("temp home path too long for sun_path: %s", p)
	}
	return home
}

// pidFromOutput extracts the pid=N field from a lifecycle verb's
// output line.
func pidFromOutput(t *testing.T, out, context string) int {
	t.Helper()
	m := regexp.MustCompile(`pid=(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("%s: no pid=N in output: %q", context, out)
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 {
		t.Fatalf("%s: bad pid %q in output: %q", context, m[1], out)
	}
	return pid
}

// TestDaemonLifecycle_RoundTrip drives the real binary through the
// full operator loop: status (down), start, status (up), idempotent
// start, restart, stop, idempotent stop. Each verb runs as its own
// subprocess against the same isolated HOME, so daemon survival
// across CLI invocations is the detachment proof.
func TestDaemonLifecycle_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	home := shortHome(t)

	// Belt-and-suspenders: never leak a daemon process past the test,
	// even when an assertion below fails first.
	t.Cleanup(func() {
		_ = run(context.Background(), t, home, "daemon stop", nil)
	})

	// Status before anything: not running, exit code 3 (the documented
	// distinct code), both human and JSON forms.
	inv := run(ctx, t, home, "daemon status", nil)
	if inv.ExitCode != 3 {
		t.Fatalf("daemon status (down): exit %d, want 3\nstdout: %s\nstderr: %s",
			inv.ExitCode, inv.Stdout, inv.Stderr)
	}
	assertContains(t, inv.Stdout, "not running", "daemon status (down)")

	inv = run(ctx, t, home, "daemon status --json", nil)
	if inv.ExitCode != 3 {
		t.Fatalf("daemon status --json (down): exit %d, want 3\nstdout: %s", inv.ExitCode, inv.Stdout)
	}
	var down struct {
		Running bool `json:"running"`
	}
	if err := json.Unmarshal([]byte(inv.Stdout), &down); err != nil {
		t.Fatalf("daemon status --json (down): not JSON: %v\nstdout: %s", err, inv.Stdout)
	}
	if down.Running {
		t.Fatalf("daemon status --json (down): running=true, want false: %s", inv.Stdout)
	}

	// Stop with nothing running: idempotent exit 0.
	inv = run(ctx, t, home, "daemon stop", nil)
	assertExitOK(t, inv, "daemon stop (down)")
	assertContains(t, inv.Stdout, "not running", "daemon stop (down)")

	// Start: detached spawn, prints pid + socket, exits 0.
	inv = run(ctx, t, home, "daemon start", nil)
	assertExitOK(t, inv, "daemon start")
	assertContains(t, inv.Stdout, "started pid=", "daemon start")
	assertContains(t, inv.Stdout, filepath.Join(home, ".guild", "daemon.sock"), "daemon start")
	pid := pidFromOutput(t, inv.Stdout, "daemon start")

	// The start command's process is gone; the daemon it spawned must
	// still be observable from a fresh process: detachment in action.
	inv = run(ctx, t, home, "daemon status", nil)
	assertExitOK(t, inv, "daemon status (up)")
	for _, field := range []string{"running", "pid=", "version=", "uptime=", "sessions=", "embedder="} {
		assertContains(t, inv.Stdout, field, "daemon status (up)")
	}
	if got := pidFromOutput(t, inv.Stdout, "daemon status (up)"); got != pid {
		t.Fatalf("daemon status pid = %d, want %d", got, pid)
	}

	// JSON status while up.
	inv = run(ctx, t, home, "daemon status --json", nil)
	assertExitOK(t, inv, "daemon status --json (up)")
	var up struct {
		Running        bool   `json:"running"`
		PID            int    `json:"pid"`
		Version        string `json:"version"`
		ActiveSessions int    `json:"active_sessions"`
		EmbedderState  string `json:"embedder_state"`
		SocketPath     string `json:"socket_path"`
		VersionDrift   bool   `json:"version_drift"`
	}
	if err := json.Unmarshal([]byte(inv.Stdout), &up); err != nil {
		t.Fatalf("daemon status --json (up): not JSON: %v\nstdout: %s", err, inv.Stdout)
	}
	if !up.Running || up.PID != pid || up.Version == "" || up.EmbedderState == "" || up.SocketPath == "" {
		t.Fatalf("daemon status --json (up): incomplete view: %s", inv.Stdout)
	}
	if up.VersionDrift {
		t.Fatalf("daemon status --json (up): drift reported for same-binary daemon: %s", inv.Stdout)
	}

	// The detached daemon's stderr landed in ~/.guild/daemon.log.
	logPath := filepath.Join(home, ".guild", "daemon.log")
	if fi, err := os.Stat(logPath); err != nil || fi.Size() == 0 {
		t.Fatalf("daemon.log missing or empty after start (err: %v)", err)
	}

	// Idempotent start: exit 0, reports the live pid, no second spawn.
	inv = run(ctx, t, home, "daemon start", nil)
	assertExitOK(t, inv, "daemon start (again)")
	assertContains(t, inv.Stdout, "already running pid=", "daemon start (again)")
	if got := pidFromOutput(t, inv.Stdout, "daemon start (again)"); got != pid {
		t.Fatalf("idempotent start pid = %d, want %d", got, pid)
	}

	// Restart: stop then start, exit 0 with a new pid.
	inv = run(ctx, t, home, "daemon restart", nil)
	assertExitOK(t, inv, "daemon restart")
	assertContains(t, inv.Stdout, "stopped pid=", "daemon restart")
	assertContains(t, inv.Stdout, "started pid=", "daemon restart")
	m := regexp.MustCompile(`started pid=(\d+)`).FindStringSubmatch(inv.Stdout)
	if m == nil {
		t.Fatalf("daemon restart: no started pid in output: %q", inv.Stdout)
	}
	newPID, err := strconv.Atoi(m[1])
	if err != nil || newPID <= 0 {
		t.Fatalf("daemon restart: bad new pid in output: %q", inv.Stdout)
	}
	if newPID == pid {
		t.Fatalf("daemon restart: pid unchanged (%d), want a fresh process", pid)
	}

	// Stop: SIGTERM path, artifacts verifiably gone afterwards.
	inv = run(ctx, t, home, "daemon stop", nil)
	assertExitOK(t, inv, "daemon stop")
	assertContains(t, inv.Stdout, "stopped pid=", "daemon stop")
	if got := pidFromOutput(t, inv.Stdout, "daemon stop"); got != newPID {
		t.Fatalf("daemon stop pid = %d, want %d", got, newPID)
	}
	for _, name := range []string{"daemon.sock", "daemon.json"} {
		p := filepath.Join(home, ".guild", name)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after stop (stat err: %v)", p, err)
		}
	}

	// Stop again: still exit 0.
	inv = run(ctx, t, home, "daemon stop", nil)
	assertExitOK(t, inv, "daemon stop (again)")
	assertContains(t, inv.Stdout, "not running", "daemon stop (again)")
}
