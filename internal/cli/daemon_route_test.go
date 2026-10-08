//go:build unix

package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/daemon/testsupport"
	"github.com/mathomhaus/guild/internal/storage"
)

// swapRouteProbe installs fn as the routing probe and resets the
// probe-once state, restoring both on cleanup. Every routing test goes
// through here so the package default (the TestMain not-running stub)
// is always reinstated.
func swapRouteProbe(t *testing.T, fn func(string, time.Duration) (daemon.ProbeResult, daemon.Discovery, error)) {
	t.Helper()
	prev := routeProbeFn
	routeProbeFn = fn
	resetDaemonRouteForTest()
	t.Cleanup(func() {
		routeProbeFn = prev
		resetDaemonRouteForTest()
	})
}

// captureRouteNudge redirects the skew-nudge writer and TTY check,
// restoring them on cleanup.
func captureRouteNudge(t *testing.T, tty bool) *bytes.Buffer {
	t.Helper()
	buf := new(bytes.Buffer)
	prevW, prevTTY := routeStderr, routeIsTTYFn
	routeStderr = buf
	routeIsTTYFn = func() bool { return tty }
	t.Cleanup(func() {
		routeStderr = prevW
		routeIsTTYFn = prevTTY
	})
	return buf
}

// seedRouteHome builds a hermetic HOME carrying quest.db and lore.db,
// each with one registered project row, and points the process at it.
func seedRouteHome(t *testing.T, projectID string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".guild"), 0o700); err != nil {
		t.Fatalf("mkdir .guild: %v", err)
	}
	ctx := context.Background()
	for _, name := range []string{"quest.db", "lore.db"} {
		db, err := storage.Open(ctx, filepath.Join(home, ".guild", name))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		if err := storage.Migrate(ctx, db, "test"); err != nil {
			_ = db.Close()
			t.Fatalf("migrate %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
			projectID, "/fake/"+projectID, "TASKS.md",
		); err != nil {
			_ = db.Close()
			t.Fatalf("seed project in %s: %v", name, err)
		}
		_ = db.Close()
	}
	return home
}

// routeLogBuffer is a goroutine-safe sink for the daemon's slog lines.
type routeLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *routeLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *routeLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startRouteDaemon runs an in-process daemon serving the production
// JSON-exec dispatch (NewDaemonExecHandler) on a short-path socket,
// writing discovery into the current HOME. Returns its log buffer.
func startRouteDaemon(t *testing.T) *routeLogBuffer {
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

	logBuf := &routeLogBuffer{}
	srv, err := daemon.NewServer(daemon.Config{
		Version:    buildVersion,
		SocketPath: sock,
		Sessions: func(_ context.Context, _ daemon.ShimPreamble, conn io.ReadWriteCloser) error {
			_, _ = io.Copy(io.Discard, conn)
			return nil
		},
		Exec:   NewDaemonExecHandler(nil, nil),
		Logger: slog.New(slog.NewTextHandler(logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop within 5s")
		}
	})

	testsupport.WaitReady(t, "socket "+sock+" dialable and discovery written", func() bool {
		select {
		case runErr := <-errCh:
			t.Fatalf("daemon exited during startup: %v", runErr)
		default:
		}
		conn, derr := net.DialTimeout("unix", sock, 100*time.Millisecond)
		if derr != nil {
			return false
		}
		_ = conn.Close()
		d, rerr := daemon.ReadDiscovery()
		return rerr == nil && d != nil
	})
	return logBuf
}

// stepOutput captures one CLI invocation's observable surface.
type stepOutput struct {
	stdout string
	stderr string
	errMsg string
}

// scrubTimes blanks minute-resolution UTC timestamps (quest scroll's
// claimed/journal lines) so two phases run minutes apart still compare.
var scrubTimes = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)

func scrub(s string) string { return scrubTimes.ReplaceAllString(s, "<TS>") }

// parityScenario is the representative verb set: writes and reads,
// quest and lore, success and typed-error paths.
func parityScenario() [][]string {
	return [][]string{
		{"quest", "post", "--project", "routeproj", "--files", "a.go", "--files", "b.go", "--acceptance", "renders identically", "parity check task"},
		{"quest", "accept", "QUEST-1", "--project", "routeproj"},
		{"quest", "journal", "QUEST-1", "--project", "routeproj", "first parity note"},
		{"quest", "list", "--project", "routeproj", "--files"},
		{"quest", "scroll", "QUEST-1", "--project", "routeproj"},
		{"lore", "inscribe", "parity lore entry title", "--project", "routeproj", "--kind", "decision", "--summary", "round trip summary", "--topic", "parity"},
		{"lore", "list", "--project", "routeproj"},
		// Typed-error paths: already-claimed and not-found narrations.
		{"quest", "accept", "QUEST-1", "--project", "routeproj"},
		{"quest", "accept", "QUEST-999", "--project", "routeproj"},
	}
}

// runScenario executes the parity scenario against a fresh HOME,
// either direct (probe stubbed not-running) or routed through a live
// in-process daemon. Returns per-step outputs plus the daemon log.
func runScenario(t *testing.T, routed bool) (steps []stepOutput, daemonLog string) {
	t.Helper()
	seedRouteHome(t, "routeproj")
	var logBuf *routeLogBuffer
	if routed {
		logBuf = startRouteDaemon(t)
		swapRouteProbe(t, daemon.Probe) // the real discovery probe
	} else {
		swapRouteProbe(t, func(string, time.Duration) (daemon.ProbeResult, daemon.Discovery, error) {
			return daemon.NotRunning, daemon.Discovery{}, nil
		})
	}

	var outs []stepOutput
	for _, args := range parityScenario() {
		stdout, stderr, err := runQuest(t, args)
		s := stepOutput{stdout: scrub(stdout), stderr: scrub(stderr)}
		if err != nil {
			s.errMsg = err.Error()
		}
		outs = append(outs, s)
	}
	if logBuf == nil {
		return outs, ""
	}
	return outs, logBuf.String()
}

// TestDaemonRoute_GoldenParityWithDirectRun is the acceptance pin:
// every representative verb produces byte-identical stdout/stderr and
// error strings whether it executes in-process or inside the daemon,
// and the routed run leaves a daemon-side execution record per verb.
func TestDaemonRoute_GoldenParityWithDirectRun(t *testing.T) {
	direct, _ := runScenario(t, false)
	routed, daemonLog := runScenario(t, true)

	steps := parityScenario()
	for i := range steps {
		if routed[i].stdout != direct[i].stdout {
			t.Errorf("step %v stdout diverged:\n--- direct ---\n%s\n--- routed ---\n%s",
				steps[i], direct[i].stdout, routed[i].stdout)
		}
		if routed[i].stderr != direct[i].stderr {
			t.Errorf("step %v stderr diverged:\n--- direct ---\n%s\n--- routed ---\n%s",
				steps[i], direct[i].stderr, routed[i].stderr)
		}
		if routed[i].errMsg != direct[i].errMsg {
			t.Errorf("step %v error diverged: direct=%q routed=%q", steps[i], direct[i].errMsg, routed[i].errMsg)
		}
	}

	// The handler ran inside the daemon process: one exec record per
	// step, with the success/handler_error split the scenario implies.
	if got := strings.Count(daemonLog, `msg="daemon: exec"`); got != len(steps) {
		t.Errorf("daemon exec records = %d; want %d\n%s", got, len(steps), daemonLog)
	}
	for _, tool := range []string{"quest_post", "quest_accept", "quest_journal", "quest_list", "quest_scroll", "lore_inscribe", "lore_list"} {
		if !strings.Contains(daemonLog, "tool="+tool) {
			t.Errorf("daemon log missing exec record for %s", tool)
		}
	}
	if got := strings.Count(daemonLog, "outcome=handler_error"); got != 2 {
		t.Errorf("handler_error records = %d; want 2 (double accept + missing quest)", got)
	}
	if got := strings.Count(daemonLog, "outcome=ok"); got != len(steps)-2 {
		t.Errorf("ok records = %d; want %d", got, len(steps)-2)
	}
}

// TestDaemonRoute_CwdProjectResolutionInsideDaemon pins the identity
// impersonation contract: with no --project flag, the daemon resolves
// the project from the CLIENT's cwd (git toplevel), never its own.
func TestDaemonRoute_CwdProjectResolutionInsideDaemon(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := seedRouteHome(t, "placeholder")

	// A real git repo inside HOME, registered under its git-reported
	// toplevel (macOS tmpdirs resolve through /private symlinks).
	repo := filepath.Join(home, "cwdrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	top, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git toplevel: %v", err)
	}
	toplevel := strings.TrimSpace(string(top))

	ctx := context.Background()
	for _, name := range []string{"quest.db", "lore.db"} {
		db, err := storage.Open(ctx, filepath.Join(home, ".guild", name))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
			"cwdrepo", toplevel, "TASKS.md",
		); err != nil {
			_ = db.Close()
			t.Fatalf("register cwdrepo: %v", err)
		}
		_ = db.Close()
	}

	logBuf := startRouteDaemon(t)
	swapRouteProbe(t, daemon.Probe)
	t.Chdir(repo)

	stdout, stderr, err := runQuest(t, []string{"quest", "post", "cwd resolved task"})
	if err != nil {
		t.Fatalf("post: %v (stderr=%q)", err, stderr)
	}
	if !strings.Contains(stdout, "posted QUEST-1: cwd resolved task") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(logBuf.String(), "tool=quest_post") {
		t.Fatalf("verb did not execute in the daemon:\n%s", logBuf.String())
	}

	// The row landed under the cwd-resolved project id.
	db, err := storage.Open(ctx, filepath.Join(home, ".guild", "quest.db"))
	if err != nil {
		t.Fatalf("open quest.db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var pid string
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM task_status WHERE task_id = 'QUEST-1'`).Scan(&pid); err != nil {
		t.Fatalf("read task project: %v", err)
	}
	if pid != "cwdrepo" {
		t.Fatalf("task project = %q; want cwdrepo (client-cwd resolution)", pid)
	}
}

// TestDaemonRoute_DaemonDownProbesOnceAndRunsLocal pins the daemon-down
// budget: one probe per process, zero behavior change.
func TestDaemonRoute_DaemonDownProbesOnceAndRunsLocal(t *testing.T) {
	seedRouteHome(t, "routeproj")
	var probes atomic.Int32
	swapRouteProbe(t, func(string, time.Duration) (daemon.ProbeResult, daemon.Discovery, error) {
		probes.Add(1)
		return daemon.NotRunning, daemon.Discovery{}, nil
	})

	stdout, _, err := runQuest(t, []string{"quest", "post", "--project", "routeproj", "local task"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !strings.Contains(stdout, "posted QUEST-1: local task") {
		t.Fatalf("stdout = %q", stdout)
	}
	if _, _, err := runQuest(t, []string{"quest", "list", "--project", "routeproj"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("probe ran %d times across two verbs; want 1", got)
	}
}

// TestDaemonRoute_VersionMismatchNudgesOnceOnTTY pins the skew rule:
// silent local execution, at most one stderr nudge, TTY-gated.
func TestDaemonRoute_VersionMismatchNudgesOnceOnTTY(t *testing.T) {
	seedRouteHome(t, "routeproj")
	swapRouteProbe(t, func(string, time.Duration) (daemon.ProbeResult, daemon.Discovery, error) {
		return daemon.RunningMismatch, daemon.Discovery{Version: "v9.9.9", SocketPath: "/nope"}, nil
	})
	nudges := captureRouteNudge(t, true)

	for i, args := range [][]string{
		{"quest", "post", "--project", "routeproj", "mismatch task"},
		{"quest", "list", "--project", "routeproj"},
	} {
		if _, _, err := runQuest(t, args); err != nil {
			t.Fatalf("verb %d: %v", i, err)
		}
	}

	want := daemon.FormatSkewNudge("v9.9.9", buildVersion) + "\n"
	if nudges.String() != want {
		t.Fatalf("nudge output = %q; want exactly one line %q", nudges.String(), want)
	}
}

func TestDaemonRoute_VersionMismatchSilentWithoutTTY(t *testing.T) {
	seedRouteHome(t, "routeproj")
	swapRouteProbe(t, func(string, time.Duration) (daemon.ProbeResult, daemon.Discovery, error) {
		return daemon.RunningMismatch, daemon.Discovery{Version: "v9.9.9", SocketPath: "/nope"}, nil
	})
	nudges := captureRouteNudge(t, false)

	stdout, _, err := runQuest(t, []string{"quest", "post", "--project", "routeproj", "silent task"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !strings.Contains(stdout, "posted QUEST-1: silent task") {
		t.Fatalf("stdout = %q", stdout)
	}
	if nudges.String() != "" {
		t.Fatalf("nudge emitted without a TTY: %q", nudges.String())
	}
}

// TestDaemonRoute_TransportFailureRetriesOnceThenRunsLocal pins the
// remote-failure contract: a daemon that drops every connection costs
// exactly one retry, then the verb executes locally and succeeds.
func TestDaemonRoute_TransportFailureRetriesOnceThenRunsLocal(t *testing.T) {
	seedRouteHome(t, "routeproj")

	dir, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var accepts atomic.Int32
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			accepts.Add(1)
			_ = conn.Close() // drop every exec attempt
		}
	}()

	swapRouteProbe(t, func(string, time.Duration) (daemon.ProbeResult, daemon.Discovery, error) {
		return daemon.RunningMatch, daemon.Discovery{Version: buildVersion, SocketPath: sock}, nil
	})

	stdout, _, err := runQuest(t, []string{"quest", "post", "--project", "routeproj", "fallback task"})
	if err != nil {
		t.Fatalf("post must still succeed locally: %v", err)
	}
	if !strings.Contains(stdout, "posted QUEST-1: fallback task") {
		t.Fatalf("stdout = %q", stdout)
	}
	if got := accepts.Load(); got != 2 {
		t.Fatalf("exec attempts = %d; want 2 (initial + one retry)", got)
	}
}
