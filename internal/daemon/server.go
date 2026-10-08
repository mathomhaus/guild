package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mathomhaus/guild/internal/module"
)

// ErrAlreadyRunning is returned by [Server.Run] when the startup probe
// finds a live daemon (any version) already serving the socket. The
// second daemon must exit non-zero without disturbing the first; the
// wrapped message carries the live daemon's pid and version.
var ErrAlreadyRunning = errors.New("daemon: a guild daemon is already running")

// ErrUnsupportedPlatform is returned by [Server.Run] on platforms where
// the unix-socket listener is not built (see listen_windows.go). The
// daemon package still compiles there so probers and the CLI surface
// keep working; only serving is gated.
var ErrUnsupportedPlatform = errors.New("daemon: serving MCP over a unix socket is not supported on this platform")

// startupProbeTimeout bounds the liveness dial Run performs before
// claiming the socket. Local unix dials complete in microseconds; one
// second is generous headroom for a heavily loaded machine.
const startupProbeTimeout = time.Second

// shutdownIdleTimeout bounds how long Run waits for write-side
// teardown of per-connection goroutines after cancellation closed
// every connection. Purely defensive; the normal path completes
// immediately.
const shutdownIdleTimeout = 10 * time.Second

// SessionHandler serves one MCP session over conn after the daemon has
// consumed the shim preamble. The handler owns conn until it returns;
// the daemon closes conn afterwards regardless. Implementations must
// honor ctx cancellation: it is the daemon's shutdown signal.
//
// The production handler is internal/mcp's DaemonHost.ServeSession; the
// indirection exists because this package is a deliberate leaf (see the
// package comment in discovery.go) and must not import internal/mcp.
type SessionHandler func(ctx context.Context, shim ShimPreamble, conn io.ReadWriteCloser) error

// Config configures a foreground daemon server.
type Config struct {
	// Version is the daemon's ldflags-stamped build version, recorded
	// in daemon.json and compared (exact string equality) by every
	// prober. Empty defaults to "dev", matching cmd/guild's unstamped
	// builds.
	Version string

	// SocketPath overrides the listen address. Empty means the
	// canonical [SocketPath] (~/.guild/daemon.sock). Tests use short
	// explicit paths because macOS caps sun_path near 104 bytes and
	// t.TempDir() under /var/folders can exceed it.
	SocketPath string

	// Sessions serves each shim connection. Required.
	Sessions SessionHandler

	// Exec executes one terminal CLI verb on behalf of a routed client
	// (JSON-exec RPC, see execrpc.go). Nil answers exec preambles with
	// a dispatch error so clients fall back to local execution.
	Exec ExecHandler

	// EmbedderState supplies the embedder_state field of the status
	// response. Nil reports "unknown".
	EmbedderState func(ctx context.Context) string

	// Scheduler is the idle dream-pass scheduler (ADR-005 Phase 2). When
	// non-nil, Run drives its loop for the daemon's lifetime and every
	// served session/exec touches it as activity. Nil means a daemon
	// that serves but never dreams (the minimal Phase 1 behavior).
	Scheduler *Scheduler

	// Pipeline is the watch -> staleness -> renewal pipeline (ADR-005
	// Phase 4). When non-nil, Run drives its watcher loop for the daemon's
	// lifetime so file/git activity under registered project roots becomes
	// lore staleness signals and renewal quests. Nil (or a disabled
	// Pipeline) means a daemon that never watches; staleness then falls
	// back to the query-time check, byte-identical to the no-daemon path.
	// Its counters are surfaced on the status line.
	Pipeline *Pipeline

	// Registry is the session registry + lease-heartbeat tick (ADR-005
	// Phase 3). When non-nil, Run drives its tick for the daemon's
	// lifetime: it renews every pre-existing lease once on boot, then
	// heartbeats each live session's leases on the configured interval so
	// a crashed agent's lease lapses while a live one never does. The
	// session handler (the host's ServeSession) registers and unregisters
	// each connection on it. Nil means a daemon that serves without ever
	// heartbeating; leases then rely on their TTL alone, the same minimal
	// behavior as a Phase 1 daemon.
	Registry *Registry

	// Reaper is the lease reaper (ADR-005 Phase 3). When non-nil, Run
	// drives its sweep tick for the daemon's lifetime: each sweep forfeits
	// the zombie claim behind any expired lease whose session has gone
	// (the agent crashed) so the work returns to the board instead of
	// rotting in_progress. It scans task_leases only, so a claim accepted
	// without the daemon is never touched. Nil means a daemon that serves
	// without ever reaping; lapsed leases then linger until something else
	// (a re-accept, a manual forfeit) clears them.
	Reaper *Reaper

	// Services are background loops contributed by enabled capability
	// modules (ADR-006 Phase 3). Run starts each on boot and stops each on
	// shutdown, alongside the built-in scheduler/pipeline/registry/reaper
	// loops, via the uniform Service registry (see service.go). Empty today
	// (no shipped module contributes a loop), so daemon startup is
	// byte-identical to the pre-Phase-3 ladder; the field exists so a
	// future module's loop joins the daemon automatically. The built-in
	// loops keep their own typed fields above because the status surface
	// reads them (Touch, watchStatus, Snapshot, TotalForfeited); only their
	// start/stop is generalized.
	Services []module.Service

	// Logger receives the daemon's structured stderr log lines. Nil
	// falls back to slog.Default(). Never log to stdout: the daemon
	// owns no protocol stream on stdout, and keeping it silent leaves
	// the channel free for future foreground tooling.
	Logger *slog.Logger
}

// Server is the foreground guild daemon: one unix-socket listener,
// one MCP session per accepted shim connection, one shared provider
// bundle behind all of them (supplied by the SessionHandler's closure,
// not owned here). Construct with [NewServer], drive with [Run].
type Server struct {
	cfg Config
	log *slog.Logger

	// startedAt is stamped by Run just before the discovery file is
	// written. Connections are only accepted after that point, so
	// status reads need no synchronization.
	startedAt time.Time

	// active counts in-flight MCP sessions (status connections are
	// never counted).
	active atomic.Int64
}

// NewServer validates cfg and returns a runnable daemon server.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("daemon: Config.Sessions handler is required")
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, log: log}, nil
}

// Run serves the daemon in the foreground until ctx is cancelled
// (SIGINT/SIGTERM via signal.NotifyContext in cmd/guild/daemon.go) or
// the listener fails. Startup order:
//
//  1. Probe for a live daemon. Any live daemon (matching version or
//     not) means refusal with [ErrAlreadyRunning]; the probe itself
//     removes stale daemon.json records (dead pid, undialable socket).
//  2. Remove a leftover socket inode. A crashed daemon cannot unlink
//     its socket, and bind fails on the stale path; reaching this step
//     means nothing live owns it (stale-socket takeover).
//  3. Listen, chmod the socket 0600, write daemon.json.
//  4. Accept loop: one goroutine per connection.
//
// On every exit path the socket and daemon.json are removed, so a
// cleanly stopped daemon never wedges the next start. A cancelled ctx
// is the operator-requested shutdown and returns nil.
func (s *Server) Run(ctx context.Context) error {
	socketPath := s.cfg.SocketPath
	if socketPath == "" {
		sp, err := SocketPath()
		if err != nil {
			return err
		}
		socketPath = sp
	}

	res, live, err := Probe(s.cfg.Version, startupProbeTimeout)
	if err != nil {
		return fmt.Errorf("daemon: startup probe: %w", err)
	}
	if res != NotRunning {
		return fmt.Errorf("%w: pid %d (version %s) on %s; stop it before starting another",
			ErrAlreadyRunning, live.PID, live.Version, live.SocketPath)
	}

	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale socket %s: %w", socketPath, err)
	}

	ln, err := listenSocket(socketPath)
	if err != nil {
		return err
	}

	if err := os.Chmod(socketPath, socketFileMode); err != nil {
		_ = ln.Close()
		s.removeArtifacts(socketPath)
		return fmt.Errorf("daemon: chmod %s: %w", socketPath, err)
	}

	s.startedAt = time.Now().UTC()
	if err := WriteDiscovery(Discovery{
		PID:        os.Getpid(),
		Version:    s.cfg.Version,
		SocketPath: socketPath,
		StartedAt:  s.startedAt,
	}); err != nil {
		_ = ln.Close()
		s.removeArtifacts(socketPath)
		return err
	}

	s.log.Info("daemon: listening",
		"socket", socketPath,
		"pid", os.Getpid(),
		"version", s.cfg.Version,
	)

	// connCtx is the per-connection lifetime: cancelled on EVERY Run
	// exit path (not just operator ctx-cancel) so a listener failure
	// also tears down in-flight sessions instead of leaking them.
	connCtx, cancelConns := context.WithCancel(ctx)
	defer cancelConns()

	var wg sync.WaitGroup

	// ── background service loops (ADR-006 Phase 3 service registry) ──────
	// The fixed `if cfg.X != nil { go ... }` ladder for the idle scheduler
	// (ADR-005 Phase 2), the watch -> staleness -> renewal pipeline (Phase
	// 4), the session/lease registry and the lease reaper (Phase 3) is now
	// a single range over the uniform Service list collectServices builds,
	// plus any loop an enabled capability module contributes (cfg.Services).
	// Every loop shares connCtx, so daemon shutdown cancels them alongside
	// the connections, byte-identical to the per-loop ladder; the built-in
	// loops keep their historical start order (scheduler, pipeline,
	// registry, reaper) so startup behavior does not move. Each service's
	// Start spawns its own goroutine; the daemon Stops each on shutdown.
	services := s.collectServices()
	for _, svc := range services {
		if err := svc.Start(connCtx); err != nil {
			s.log.Warn("daemon: service failed to start", "service", svc.Name(), "err", err)
		}
	}

	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.serveConn(connCtx, conn)
			}()
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		// Operator-requested shutdown: the expected clean-stop path.
	case err := <-acceptErr:
		runErr = fmt.Errorf("daemon: accept: %w", err)
	}

	_ = ln.Close() // unlinks the socket; pending Accept returns
	cancelConns()  // sessions + service loops observe cancellation

	// Stop each background service: connCtx is already cancelled, so each
	// loop is winding down; Stop joins its goroutine, bounded by the same
	// shutdownIdleTimeout the connection wait uses so a loop ignoring
	// cancellation cannot wedge shutdown forever.
	s.stopServices(services)

	waitWithTimeout(&wg, shutdownIdleTimeout, s.log)
	s.removeArtifacts(socketPath)

	s.log.Info("daemon: stopped", "pid", os.Getpid())
	return runErr
}

// stopServices stops every background service, bounding the total drain by
// shutdownIdleTimeout and logging any loop that does not return in time.
// The services' Start ctx (connCtx) is already cancelled when this runs,
// so each loop should be returning; Stop only joins it.
//
// The drain ctx is deliberately detached from any caller ctx
// (context.Background with a fresh timeout): shutdown is reached precisely
// because the run ctx was cancelled, so deriving the drain deadline from
// it would expire instantly and defeat the bounded join. Same rationale as
// waitWithTimeout's wall-clock bound.
//
//nolint:contextcheck // drain deadline must outlive the already-cancelled run ctx
func (s *Server) stopServices(services []module.Service) {
	if len(services) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownIdleTimeout)
	defer cancel()
	for _, svc := range services {
		if err := svc.Stop(ctx); err != nil {
			s.log.Warn("daemon: service did not stop cleanly", "service", svc.Name(), "err", err)
		}
	}
}

// serveConn handles one accepted connection end to end: preamble,
// then either a one-line status response or a full MCP session.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// Tie the raw connection to daemon shutdown. The SDK layer also
	// honors ctx, but the preamble read below happens before any SDK
	// involvement; closing the conn is what unblocks it.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	// The preamble must arrive promptly: the discovery probe dials and
	// closes without writing a byte, and a wedged client must not pin
	// this goroutine.
	_ = conn.SetReadDeadline(time.Now().Add(preambleTimeout))
	br := bufio.NewReader(conn)
	pre, err := readPreamble(br)
	if err != nil {
		// An instant EOF is the liveness probe's dial-and-close;
		// routine, not noteworthy.
		if !errors.Is(err, io.EOF) {
			s.log.Warn("daemon: dropping connection with bad preamble", "err", err)
		}
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if pre.StatusRequest != nil {
		s.writeStatus(ctx, conn)
		return
	}

	if pre.Exec != nil {
		// Exec connections are one-shot RPCs, not MCP sessions: they
		// never touch the active-session count.
		s.serveExec(ctx, br, conn, *pre.Exec)
		return
	}

	shim := *pre.Shim
	// Activity for the idle scheduler: a session attaching is the
	// clearest "the operator is awake" signal the daemon layer sees, and
	// it preempts any in-flight dream pass so the new session never waits
	// on dreaming. The session-end touch below brackets the session so
	// the idle clock starts from when the LAST session detached, not from
	// when it attached.
	s.touch()
	s.log.Info("daemon: session start",
		"shim_pid", shim.PID,
		"shim_version", shim.Version,
		"cwd", shim.CWD,
		"active", s.active.Add(1),
	)
	defer func() {
		s.touch()
		s.log.Info("daemon: session end",
			"shim_pid", shim.PID,
			"active", s.active.Add(-1),
		)
	}()

	if err := s.cfg.Sessions(ctx, shim, &sessionConn{r: br, conn: conn}); err != nil && ctx.Err() == nil {
		s.log.Warn("daemon: session ended with error", "shim_pid", shim.PID, "err", err)
	}
}

// writeStatus answers a status-request preamble: one JSON line, then
// the deferred close in serveConn ends the connection.
func (s *Server) writeStatus(ctx context.Context, conn net.Conn) {
	state := "unknown"
	if s.cfg.EmbedderState != nil {
		state = s.cfg.EmbedderState(ctx)
	}
	st := Status{
		PID:            os.Getpid(),
		Version:        s.cfg.Version,
		StartedAt:      s.startedAt,
		ActiveSessions: int(s.active.Load()),
		EmbedderState:  state,
		Watch:          s.watchStatus(),
		Sessions:       s.sessionStatuses(),
		LeasesReaped:   s.leasesReaped(),
	}
	data, err := json.Marshal(st)
	if err != nil {
		s.log.Warn("daemon: marshal status", "err", err)
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(preambleTimeout))
	if _, err := conn.Write(append(data, '\n')); err != nil {
		s.log.Warn("daemon: write status", "err", err)
	}
}

// touch records activity on the idle scheduler when one is wired. A nil
// Scheduler (minimal Phase 1 daemon) makes it a no-op, so the activity
// hooks at the session and exec entry points cost nothing when sleep is
// not configured.
func (s *Server) touch() {
	if s.cfg.Scheduler != nil {
		s.cfg.Scheduler.Touch()
	}
}

// watchStatus maps the wired Pipeline's snapshot onto the status wire
// shape. A nil Pipeline (or one never started) reports a disabled watcher
// with zero counters, so the status line is always populated.
func (s *Server) watchStatus() WatchStatus {
	if s.cfg.Pipeline == nil {
		return WatchStatus{}
	}
	ps := s.cfg.Pipeline.Status()
	return WatchStatus{
		Enabled:         ps.Enabled,
		Watching:        ps.Watching,
		ProjectsWatched: ps.ProjectsWatched,
		EventsSeen:      ps.EventsSeen,
		SignalsRecorded: ps.SignalsRecorded,
		QuestsPosted:    ps.QuestsPosted,
		LastError:       ps.LastError,
	}
}

// waitWithTimeout joins wg but gives up after d, logging instead of
// hanging shutdown forever on a handler that ignored cancellation.
func waitWithTimeout(wg *sync.WaitGroup, d time.Duration, log *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		log.Warn("daemon: shutdown timed out waiting for sessions to end", "timeout", d.String())
	}
}

// removeArtifacts deletes the socket and discovery file best-effort.
// Both paths must be gone on every Run exit so a crashed-then-restarted
// daemon never inherits a wedged state.
func (s *Server) removeArtifacts(socketPath string) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("daemon: remove socket on shutdown", "socket", socketPath, "err", err)
	}
	removeDiscovery()
}
