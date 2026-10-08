//go:build unix

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/daemon/testsupport"
)

// shortSocketPath and setHome live in discovery_test.go and are shared
// here: sockets in a short-named temp dir (sun_path cap), HOME in a
// hermetic t.TempDir().

// quietLogger keeps daemon log lines out of test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// blockingHandler is a SessionHandler that holds the session open until
// the peer (or daemon shutdown) closes the connection.
func blockingHandler(_ context.Context, _ ShimPreamble, conn io.ReadWriteCloser) error {
	buf := make([]byte, 64)
	for {
		if _, err := conn.Read(buf); err != nil {
			return nil
		}
	}
}

// startDaemon runs srv in a goroutine and blocks until the socket
// accepts a connection and the discovery file is on disk. Returns the
// channel Run's result lands on.
func startDaemon(t *testing.T, ctx context.Context, srv *Server, socketPath string) chan error {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	// Registered after temporary HOME/socket cleanup, so LIFO ordering joins
	// Run before either artifact location can be restored or removed. The
	// dedicated done signal remains available even if a test consumes errCh.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("daemon Run did not finish during test cleanup")
		}
	})
	go func() { defer close(done); errCh <- srv.Run(runCtx) }()

	testsupport.WaitReady(t, "socket "+socketPath+" dialable and discovery written", func() bool {
		select {
		case runErr := <-errCh:
			t.Fatalf("daemon exited during startup: %v", runErr)
		default:
		}
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		d, derr := ReadDiscovery()
		return derr == nil && d != nil
	})
	return errCh
}

// requestStatus dials the daemon, sends a status-request preamble, and
// decodes the one-line response, asserting the daemon closes the
// connection afterwards.
func requestStatus(t *testing.T, socketPath string) Status {
	t.Helper()
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("dial for status: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(`{"guild_status_request":{}}` + "\n")); err != nil {
		t.Fatalf("write status request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	var st Status
	if err := json.Unmarshal(line, &st); err != nil {
		t.Fatalf("parse status %q: %v", line, err)
	}
	// One line, then close: the next read must be EOF.
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("status conn not closed after response; read err = %v", err)
	}
	return st
}

func TestRun_DiscoveryLifecycleAndSocketPerms(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := NewServer(Config{
		Version:    "v-test",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := startDaemon(t, ctx, srv, sock)

	// daemon.json appears on successful listen, carrying this process.
	d, err := ReadDiscovery()
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if d == nil {
		t.Fatal("daemon.json missing while daemon is listening")
	}
	if d.PID != os.Getpid() || d.Version != "v-test" || d.SocketPath != sock {
		t.Fatalf("discovery = %+v; want pid=%d version=v-test socket=%s", d, os.Getpid(), sock)
	}
	if d.StartedAt.IsZero() {
		t.Fatal("discovery started_at is zero")
	}

	// Socket exists, is a socket, and is 0600 inside the temp dir.
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %v)", sock, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perm = %o; want 0600", perm)
	}

	// Clean shutdown: ctx cancel returns nil and removes both artifacts.
	cancel()
	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("Run returned %v on ctx cancel; want nil", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of ctx cancel")
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still present after shutdown: %v", err)
	}
	if d, err := ReadDiscovery(); err != nil || d != nil {
		t.Fatalf("daemon.json still present after shutdown: d=%+v err=%v", d, err)
	}
}

func TestRun_SecondDaemonRefusedWhileFirstLive(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, err := NewServer(Config{
		Version:    "v-one",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer first: %v", err)
	}
	startDaemon(t, ctx, first, sock)

	// Same version and a DIFFERENT version both refuse: any live daemon
	// blocks a second instance.
	for _, version := range []string{"v-one", "v-two"} {
		second, err := NewServer(Config{
			Version:    version,
			SocketPath: shortSocketPath(t),
			Sessions:   blockingHandler,
			Logger:     quietLogger(),
		})
		if err != nil {
			t.Fatalf("NewServer second (%s): %v", version, err)
		}
		runErr := second.Run(ctx)
		if !errors.Is(runErr, ErrAlreadyRunning) {
			t.Fatalf("second Run (%s) = %v; want ErrAlreadyRunning", version, runErr)
		}
	}

	// The refusal did not disturb the first daemon: discovery intact,
	// status still answers.
	d, err := ReadDiscovery()
	if err != nil || d == nil || d.Version != "v-one" || d.SocketPath != sock {
		t.Fatalf("first daemon's discovery disturbed: d=%+v err=%v", d, err)
	}
	st := requestStatus(t, sock)
	if st.Version != "v-one" || st.PID != os.Getpid() {
		t.Fatalf("first daemon unhealthy after refusals: %+v", st)
	}
}

func TestRun_StaleSocketTakeover(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	// Fabricate a crash: a socket inode with no listener behind it,
	// plus a discovery record whose pid is alive (this process) but
	// whose socket no longer dials. Probe must classify it stale.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close pre-listener: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket inode missing: %v", err)
	}
	if err := WriteDiscovery(Discovery{
		PID: os.Getpid(), Version: "v-crashed", SocketPath: sock, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write stale discovery: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := NewServer(Config{
		Version:    "v-next",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)

	// The next daemon took the socket over and rewrote discovery.
	st := requestStatus(t, sock)
	if st.Version != "v-next" {
		t.Fatalf("takeover daemon reports version %q; want v-next", st.Version)
	}
	d, err := ReadDiscovery()
	if err != nil || d == nil || d.Version != "v-next" {
		t.Fatalf("discovery after takeover: d=%+v err=%v", d, err)
	}
}

func TestRun_StatusReportsSessionsAndEmbedder(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := NewServer(Config{
		Version:       "v-status",
		SocketPath:    sock,
		Sessions:      blockingHandler,
		EmbedderState: func(context.Context) string { return "disabled" },
		Logger:        quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)

	// No sessions yet.
	st := requestStatus(t, sock)
	if st.PID != os.Getpid() || st.Version != "v-status" {
		t.Fatalf("status identity = %+v", st)
	}
	if st.StartedAt.IsZero() || st.StartedAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("status started_at implausible: %v", st.StartedAt)
	}
	if st.EmbedderState != "disabled" {
		t.Fatalf("embedder_state = %q; want disabled", st.EmbedderState)
	}
	if st.ActiveSessions != 0 {
		t.Fatalf("active_sessions = %d before any session; want 0", st.ActiveSessions)
	}

	// Open a shim session and hold it; the count must reach 1.
	shim, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	defer func() { _ = shim.Close() }()
	if _, err := shim.Write([]byte(`{"guild_shim":{"version":"s","cwd":"/x","pid":4242}}` + "\n")); err != nil {
		t.Fatalf("write shim preamble: %v", err)
	}
	waitForSessions(t, sock, 1)

	// Close the shim; the count must drop back to 0.
	_ = shim.Close()
	waitForSessions(t, sock, 0)
}

// waitForSessions polls the status endpoint until active_sessions
// reaches want (preamble handling is asynchronous to the dialer).
func waitForSessions(t *testing.T, sock string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := requestStatus(t, sock)
		if st.ActiveSessions == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active_sessions = %d; want %d (timed out)", st.ActiveSessions, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRun_PreambleRemainderReachesHandler(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	type captured struct {
		shim ShimPreamble
		line string
	}
	got := make(chan captured, 1)
	handler := func(_ context.Context, shim ShimPreamble, conn io.ReadWriteCloser) error {
		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		got <- captured{shim: shim, line: line}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := NewServer(Config{
		Version:    "v-splice",
		SocketPath: sock,
		Sessions:   handler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)

	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Preamble and first JSON-RPC frame in ONE write so the daemon's
	// preamble reader is guaranteed to over-read into its buffer.
	payload := `{"guild_shim":{"version":"s","cwd":"/work/p","pid":777}}` + "\n" +
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	select {
	case c := <-got:
		if c.shim.PID != 777 || c.shim.CWD != "/work/p" || c.shim.Version != "s" {
			t.Fatalf("handler saw shim %+v", c.shim)
		}
		want := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"
		if c.line != want {
			t.Fatalf("handler first line = %q; want %q", c.line, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never received the post-preamble frame")
	}
}

func TestRun_BadPreambleDropsConnOnly(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := NewServer(Config{
		Version:    "v-bad",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)

	// Garbage preamble: the daemon must close THIS conn and keep serving.
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("definitely not json\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("garbage conn not closed; read err = %v", err)
	}
	_ = conn.Close()

	// Probe-style dial-and-close (zero bytes) is routine, not fatal.
	probe, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	_ = probe.Close()

	st := requestStatus(t, sock)
	if st.Version != "v-bad" {
		t.Fatalf("daemon unhealthy after bad preambles: %+v", st)
	}
}

// TestRun_SIGTERMShutsDownCleanly drives the real signal path the
// `guild daemon run` wiring uses: signal.NotifyContext + SIGTERM to
// our own process. Run must return nil (exit 0 at the CLI layer) with
// socket and daemon.json removed.
func TestRun_SIGTERMShutsDownCleanly(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	srv, err := NewServer(Config{
		Version:    "v-sig",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := startDaemon(t, ctx, srv, sock)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("self-SIGTERM: %v", err)
	}

	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("Run returned %v on SIGTERM; want nil", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of SIGTERM")
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still present after SIGTERM: %v", err)
	}
	if d, err := ReadDiscovery(); err != nil || d != nil {
		t.Fatalf("daemon.json still present after SIGTERM: d=%+v err=%v", d, err)
	}
}

func TestNewServer_RequiresSessionsHandler(t *testing.T) {
	if _, err := NewServer(Config{Version: "v"}); err == nil {
		t.Fatal("NewServer accepted a Config without a Sessions handler")
	}
}

// TestRun_StatusReportsPresenceDetail verifies the status reply carries the
// per-session presence detail (ADR-005 Phase 3) from the wired registry and
// the lifetime reap counter from the wired reaper. The registry is populated
// directly (the production path registers on connect); the reaper's counter
// is driven by a sweep. This pins the wire shape `guild daemon status`
// renders.
func TestRun_StatusReportsPresenceDetail(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := NewRegistry(RegistryConfig{Logger: quietLogger()})
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	reg.Register("4242", "alpha", at)
	reg.SetHeldQuests("4242", []string{"QUEST-3"})

	reaper := NewReaper(ReaperConfig{Logger: quietLogger()})
	reaper.totalForfeited.Store(2) // as if two zombie claims were reaped

	srv, err := NewServer(Config{
		Version:       "v-presence",
		SocketPath:    sock,
		Sessions:      blockingHandler,
		EmbedderState: func(context.Context) string { return "disabled" },
		Registry:      reg,
		Reaper:        reaper,
		Logger:        quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)

	st := requestStatus(t, sock)
	if st.LeasesReaped != 2 {
		t.Errorf("LeasesReaped = %d, want 2", st.LeasesReaped)
	}
	if len(st.Sessions) != 1 {
		t.Fatalf("Sessions = %d, want 1: %+v", len(st.Sessions), st.Sessions)
	}
	s := st.Sessions[0]
	if s.ID != "4242" || s.Project != "alpha" {
		t.Errorf("session identity = %+v, want id 4242 project alpha", s)
	}
	if len(s.HeldQuests) != 1 || s.HeldQuests[0] != "QUEST-3" {
		t.Errorf("HeldQuests = %v, want [QUEST-3]", s.HeldQuests)
	}
	if !s.ConnectedAt.Equal(at) {
		t.Errorf("ConnectedAt = %v, want %v", s.ConnectedAt, at)
	}
}

// TestRun_StatusPresenceEmptyWhenNoRegistry verifies a daemon built without a
// registry (a Phase 1 daemon) reports no per-session detail and a zero reap
// counter, so the status line degrades cleanly to the active-count summary.
func TestRun_StatusPresenceEmptyWhenNoRegistry(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := NewServer(Config{
		Version:    "v-noreg",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)

	st := requestStatus(t, sock)
	if len(st.Sessions) != 0 {
		t.Errorf("Sessions = %+v, want empty without a registry", st.Sessions)
	}
	if st.LeasesReaped != 0 {
		t.Errorf("LeasesReaped = %d, want 0 without a reaper", st.LeasesReaped)
	}
}

// Cleanup must finish Run before t.Setenv restores HOME. Otherwise the old
// daemon's artifact removal can target the next test's newly written discovery.
func TestStartDaemonCleanupJoinsRunBeforeRestoringHome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var exited chan error
	t.Run("first home", func(t *testing.T) {
		setHome(t)
		sock := shortSocketPath(t)
		// This cleanup was registered before startDaemon's. It executes after
		// the helper joins Run, even if a consumer already drained errCh.
		t.Cleanup(func() {
			select {
			case err := <-exited:
				if err != nil {
					t.Errorf("Run exit=%v", err)
				}
			default:
				t.Error("test cleanup returned before Run exited")
			}
		})
		srv, err := NewServer(Config{Version: "first", SocketPath: sock, Sessions: blockingHandler, Logger: quietLogger()})
		if err != nil {
			t.Fatal(err)
		}
		exited = startDaemon(t, ctx, srv, sock)
	})
	t.Run("next home", func(t *testing.T) {
		setHome(t)
		sock := shortSocketPath(t)
		srv, err := NewServer(Config{Version: "next", SocketPath: sock, Sessions: blockingHandler, Logger: quietLogger()})
		if err != nil {
			t.Fatal(err)
		}
		startDaemon(t, context.Background(), srv, sock)
		// The first parent context is still alive until here. With the old
		// helper this triggered asynchronous removal against the next HOME.
		cancel()
		discovery, err := ReadDiscovery()
		if err != nil || discovery == nil || discovery.SocketPath != sock || discovery.Version != "next" {
			t.Fatalf("previous cleanup removed next discovery: %+v %v", discovery, err)
		}
	})
}

func TestStartDaemonCleanupDoesNotRequireUndrainedResult(t *testing.T) {
	t.Run("consumed result", func(t *testing.T) {
		setHome(t)
		sock := shortSocketPath(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		srv, err := NewServer(Config{Version: "consumed", SocketPath: sock, Sessions: blockingHandler, Logger: quietLogger()})
		if err != nil {
			t.Fatal(err)
		}
		result := startDaemon(t, ctx, srv, sock)
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop")
		}
		// The helper's cleanup now runs with an empty result channel. Its
		// independent done signal must still allow immediate completion.
	})
}
