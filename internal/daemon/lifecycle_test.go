//go:build unix

package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperDaemonEnv selects the helper-daemon mode TestMain runs instead
// of the test suite. The lifecycle tests re-exec this test binary as
// the "daemon", so Start/Stop are exercised against a real detached
// process serving a real socket without building the full guild
// binary.
const helperDaemonEnv = "GUILD_LIFECYCLE_TEST_DAEMON"

func TestMain(m *testing.M) {
	switch os.Getenv(helperDaemonEnv) {
	case "":
		os.Exit(m.Run())
	case "serve":
		os.Exit(runHelperDaemon(false))
	case "ignore-term":
		os.Exit(runHelperDaemon(true))
	default:
		fmt.Fprintf(os.Stderr, "unknown %s mode %q\n", helperDaemonEnv, os.Getenv(helperDaemonEnv))
		os.Exit(2)
	}
}

// runHelperDaemon stands in for `guild daemon run`: a real Server on
// the canonical socket under the (test-controlled) $HOME. ignoreTerm
// swallows SIGTERM so Stop's SIGKILL escalation path is forced.
// Returns the process exit code so TestMain owns the os.Exit.
func runHelperDaemon(ignoreTerm bool) int {
	ctx := context.Background()
	stop := func() {}
	if ignoreTerm {
		// Registering a handler disables SIGTERM's default
		// termination; the channel is deliberately never drained into
		// a cancel, so only SIGKILL ends this process.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
	} else {
		ctx, stop = signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	}
	defer stop()

	srv, err := NewServer(Config{
		Version: "dev",
		Sessions: func(ctx context.Context, _ ShimPreamble, _ io.ReadWriteCloser) error {
			<-ctx.Done()
			return nil
		},
		EmbedderState: func(context.Context) string { return "test-embedder" },
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper daemon:", err)
		return 1
	}
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "helper daemon:", err)
		return 1
	}
	return 0
}

// setShortHome points $HOME at a fresh short-named temp dir. The
// lifecycle tests put the REAL canonical socket under $HOME/.guild,
// and t.TempDir() paths embed the test name, which can blow past the
// sun_path cap on macOS; os.MkdirTemp keeps the prefix minimal.
func setShortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	if p := filepath.Join(home, ".guild", socketFileName); len(p) > 100 {
		t.Skipf("temp home path too long for sun_path: %s", p)
	}
	return home
}

// startHelper spawns the helper daemon through the production Start
// path and registers a best-effort SIGKILL cleanup so a failing test
// never leaks a daemon process.
func startHelper(t *testing.T, mode string) StartResult {
	t.Helper()
	t.Setenv(helperDaemonEnv, mode)

	res, err := Start(StartOptions{
		Exe:         os.Args[0],
		Args:        []string{}, // helper mode dispatches on env, not argv
		WaitTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if alive, aerr := processAlive(res.PID); aerr == nil && alive {
			_ = killProcess(res.PID)
		}
	})
	return res
}

func TestStartStopRoundTrip(t *testing.T) {
	home := setShortHome(t)

	res := startHelper(t, "serve")
	if res.AlreadyRunning {
		t.Fatalf("first Start reported AlreadyRunning, want fresh spawn: %+v", res)
	}
	if res.PID <= 0 {
		t.Fatalf("Start returned pid %d, want > 0", res.PID)
	}
	wantSock := filepath.Join(home, ".guild", socketFileName)
	if res.SocketPath != wantSock {
		t.Fatalf("Start socket = %q, want %q", res.SocketPath, wantSock)
	}
	if res.DaemonVersion != "dev" {
		t.Fatalf("Start daemon version = %q, want dev", res.DaemonVersion)
	}
	if res.VersionMismatch {
		t.Fatalf("Start reported version mismatch for matching versions: %+v", res)
	}
	if _, err := os.Stat(wantSock); err != nil {
		t.Fatalf("socket missing after Start: %v", err)
	}
	if d, err := ReadDiscovery(); err != nil || d == nil || d.PID != res.PID {
		t.Fatalf("discovery after Start = %+v (err %v), want pid %d", d, err, res.PID)
	}

	// Idempotent second start: no second spawn, same pid, exit-0 path.
	res2, err := Start(StartOptions{Exe: os.Args[0], Args: []string{}})
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if !res2.AlreadyRunning || res2.PID != res.PID {
		t.Fatalf("second Start = %+v, want AlreadyRunning with pid %d", res2, res.PID)
	}

	// Status while running.
	rep, err := QueryStatus("dev", time.Second)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if !rep.Running {
		t.Fatal("QueryStatus reported not running while daemon is up")
	}
	if rep.Status.PID != res.PID {
		t.Fatalf("status pid = %d, want %d", rep.Status.PID, res.PID)
	}
	if rep.Status.ActiveSessions != 0 {
		t.Fatalf("status sessions = %d, want 0", rep.Status.ActiveSessions)
	}
	if rep.Status.EmbedderState != "test-embedder" {
		t.Fatalf("status embedder = %q, want test-embedder", rep.Status.EmbedderState)
	}
	if rep.Uptime < 0 {
		t.Fatalf("status uptime = %s, want >= 0", rep.Uptime)
	}
	if rep.Drift || rep.DriftNudge != "" {
		t.Fatalf("status reported drift for matching versions: %+v", rep)
	}

	// Status from a "different binary": drift detected, nudge present.
	repDrift, err := QueryStatus("v99.0.0", time.Second)
	if err != nil {
		t.Fatalf("QueryStatus (drift): %v", err)
	}
	if !repDrift.Running || !repDrift.Drift || repDrift.DriftNudge == "" {
		t.Fatalf("drift status = %+v, want Running with non-empty DriftNudge", repDrift)
	}

	// Clean stop: SIGTERM only, daemon removes its own artifacts.
	stopRes, err := Stop(StopOptions{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopRes.WasRunning || stopRes.PID != res.PID || stopRes.Killed {
		t.Fatalf("Stop = %+v, want WasRunning pid %d without SIGKILL", stopRes, res.PID)
	}
	assertArtifactsGone(t, home)

	// Idempotent second stop.
	stop2, err := Stop(StopOptions{})
	if err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if stop2.WasRunning {
		t.Fatalf("second Stop = %+v, want WasRunning=false", stop2)
	}

	// Status after stop.
	repDown, err := QueryStatus("dev", time.Second)
	if err != nil {
		t.Fatalf("QueryStatus after stop: %v", err)
	}
	if repDown.Running {
		t.Fatal("QueryStatus reported running after Stop")
	}
}

func TestStopEscalatesToSigkill(t *testing.T) {
	home := setShortHome(t)

	res := startHelper(t, "ignore-term")

	stopRes, err := Stop(StopOptions{
		TermTimeout: 300 * time.Millisecond,
		KillTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopRes.WasRunning || stopRes.PID != res.PID {
		t.Fatalf("Stop = %+v, want WasRunning pid %d", stopRes, res.PID)
	}
	if !stopRes.Killed {
		t.Fatalf("Stop = %+v, want Killed=true for a SIGTERM-ignoring daemon", stopRes)
	}

	// SIGKILL skipped the daemon's own cleanup; Stop must have removed
	// the artifacts on its behalf.
	assertArtifactsGone(t, home)
}

func TestStartFailureTailsLog(t *testing.T) {
	home := setShortHome(t)

	_, err := Start(StartOptions{
		Exe:         "/bin/sh",
		Args:        []string{"-c", "echo boom-detail >&2; exit 3"},
		WaitTimeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("Start succeeded for a child that exits immediately")
	}
	if !strings.Contains(err.Error(), "boom-detail") {
		t.Fatalf("Start error does not carry the log tail: %v", err)
	}

	// The default log path captured the child's stderr.
	data, rerr := os.ReadFile(filepath.Join(home, ".guild", logFileName))
	if rerr != nil {
		t.Fatalf("read daemon.log: %v", rerr)
	}
	if !strings.Contains(string(data), "boom-detail") {
		t.Fatalf("daemon.log = %q, want child stderr in it", data)
	}
}

func TestStartRotatesLog(t *testing.T) {
	home := setShortHome(t)
	guildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(guildDir, 0o700); err != nil {
		t.Fatalf("mkdir .guild: %v", err)
	}
	logPath := filepath.Join(guildDir, logFileName)
	if err := os.WriteFile(logPath, []byte("old-generation\n"), 0o600); err != nil {
		t.Fatalf("seed daemon.log: %v", err)
	}

	_, err := Start(StartOptions{
		Exe:         "/bin/sh",
		Args:        []string{"-c", "echo new-generation >&2; exit 1"},
		WaitTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("Start succeeded for a child that exits immediately")
	}

	oldData, rerr := os.ReadFile(logPath + ".old")
	if rerr != nil {
		t.Fatalf("read daemon.log.old: %v", rerr)
	}
	if !strings.Contains(string(oldData), "old-generation") {
		t.Fatalf("daemon.log.old = %q, want previous generation", oldData)
	}
	newData, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read daemon.log: %v", rerr)
	}
	if strings.Contains(string(newData), "old-generation") {
		t.Fatalf("daemon.log = %q, still carries the previous generation", newData)
	}
}

func TestStopNotRunning(t *testing.T) {
	setShortHome(t)

	res, err := Stop(StopOptions{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.WasRunning {
		t.Fatalf("Stop = %+v, want WasRunning=false on a fresh home", res)
	}
}

func TestQueryStatusNotRunning(t *testing.T) {
	setShortHome(t)

	rep, err := QueryStatus("dev", time.Second)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if rep.Running {
		t.Fatalf("QueryStatus = %+v, want Running=false on a fresh home", rep)
	}
	if rep.SelfVersion != "dev" {
		t.Fatalf("QueryStatus self version = %q, want dev", rep.SelfVersion)
	}
}

func TestFormatVersionDrift(t *testing.T) {
	cases := []struct {
		name             string
		daemonV, selfV   string
		wantEmpty        bool
		wantContainsEach []string
	}{
		{name: "equal", daemonV: "v1.2.3", selfV: "v1.2.3", wantEmpty: true},
		{name: "equal dev", daemonV: "dev", selfV: "dev", wantEmpty: true},
		{
			name: "daemon newer", daemonV: "v1.3.0", selfV: "v1.2.0",
			wantContainsEach: []string{"newer than this binary", "v1.3.0", "v1.2.0"},
		},
		{
			name: "daemon older", daemonV: "v1.1.0", selfV: "v1.2.0",
			wantContainsEach: []string{"older than this binary", "guild daemon restart"},
		},
		{
			name: "indeterminate", daemonV: "dev", selfV: "v1.2.0",
			wantContainsEach: []string{"does not match this binary", "guild daemon restart"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatVersionDrift(tc.daemonV, tc.selfV)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("FormatVersionDrift(%q, %q) = %q, want empty", tc.daemonV, tc.selfV, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("FormatVersionDrift(%q, %q) = empty, want a nudge", tc.daemonV, tc.selfV)
			}
			for _, want := range tc.wantContainsEach {
				if !strings.Contains(got, want) {
					t.Errorf("FormatVersionDrift(%q, %q) = %q, missing %q", tc.daemonV, tc.selfV, got, want)
				}
			}
		})
	}
}

func TestTailLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")

	if got := tailLog(path, 100); got != "" {
		t.Fatalf("tailLog(missing) = %q, want empty", got)
	}

	var b strings.Builder
	for i := range 50 {
		fmt.Fprintf(&b, "line-%02d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got := tailLog(path, 40)
	if strings.Contains(got, "line-00") {
		t.Fatalf("tailLog = %q, want only the tail", got)
	}
	if !strings.HasSuffix(got, "line-49") {
		t.Fatalf("tailLog = %q, want it to end with the last line", got)
	}
	// The capped window starts mid-line; the partial first line must
	// have been dropped so every returned line is whole.
	if !strings.HasPrefix(got, "line-") {
		t.Fatalf("tailLog = %q, want it to start on a whole line", got)
	}
}

// assertArtifactsGone fails the test when the socket or discovery file
// survived a stop.
func assertArtifactsGone(t *testing.T, home string) {
	t.Helper()
	for _, name := range []string{socketFileName, discoveryFileName} {
		p := filepath.Join(home, ".guild", name)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after stop (stat err: %v)", p, err)
		}
	}
}
