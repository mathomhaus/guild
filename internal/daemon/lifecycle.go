package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/release"
)

// This file implements the operator-facing daemon lifecycle: Start
// (detached spawn of `guild daemon run`), Stop (SIGTERM with SIGKILL
// escalation), and QueryStatus (live status line over the socket).
// Restart is stop-then-start composed by the CLI from these helpers.
//
// Platform note: the lifecycle is unix-only for now. The package still
// compiles on windows (probers and protocol types are shared with the
// CLI surface); each entry point gates on lifecycleSupported and
// returns [ErrLifecycleUnsupported] there, so the windows binary keeps
// the no-daemon path with a clear message instead of failing to build.

// ErrLifecycleUnsupported is returned by [Start], [Stop], and
// [QueryStatus] on platforms without daemon support (currently
// windows). The text is the operator-facing message.
var ErrLifecycleUnsupported = errors.New("daemon mode is not yet supported on this platform")

// logFileName is the basename of the detached daemon's stderr log
// under ~/.guild/.
const logFileName = "daemon.log"

// logFileMode is the mode bits for ~/.guild/daemon.log. Log lines can
// carry project names and paths, so the file gets the same per-user
// treatment as daemon.json inside the 0700 dir.
const logFileMode os.FileMode = 0o600

// logTailBytes bounds how much of daemon.log a failed Start echoes
// back to the operator.
const logTailBytes = 2048

// lifecycleProbeTimeout bounds each liveness dial the lifecycle verbs
// perform. Local unix dials complete in microseconds; one second is
// generous headroom for a loaded machine.
const lifecycleProbeTimeout = time.Second

// defaultStartWait bounds how long Start polls for the spawned daemon
// to become ready before tailing the log and failing.
const defaultStartWait = 3 * time.Second

// defaultTermWait is Stop's SIGTERM grace period before escalating.
const defaultTermWait = 5 * time.Second

// defaultKillWait bounds how long Stop waits for the process to vanish
// after SIGKILL before reporting failure.
const defaultKillWait = 2 * time.Second

// startPollInterval paces Start's readiness probe loop.
const startPollInterval = 50 * time.Millisecond

// stopPollInterval paces Stop's process-gone poll loop.
const stopPollInterval = 25 * time.Millisecond

// LogPath returns the detached daemon's stderr log location
// (~/.guild/daemon.log), ensuring ~/.guild exists with 0700 first.
func LogPath() (string, error) {
	dir, err := guildpath.EnsureGuildDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve log path: %w", err)
	}
	return filepath.Join(dir, logFileName), nil
}

// StartOptions configures [Start]. The zero value is production
// behavior: re-exec the current binary with "daemon run", inherit the
// environment, log to ~/.guild/daemon.log, wait up to 3s for
// readiness. The overrides exist for tests, which substitute a helper
// process for the real binary.
type StartOptions struct {
	// SelfVersion is the invoking binary's ldflags-stamped version.
	// Empty defaults to "dev", matching unstamped builds.
	SelfVersion string

	// Exe is the program to spawn. Empty means os.Executable().
	Exe string

	// Args is the spawned program's argv after the program name. A nil
	// slice means the production argv ["daemon", "run"]; an empty
	// non-nil slice means no arguments.
	Args []string

	// Env is the spawned program's environment. Nil inherits the
	// parent's environment.
	Env []string

	// LogPath overrides where the daemon's stderr lands. Empty means
	// [LogPath] (~/.guild/daemon.log).
	LogPath string

	// WaitTimeout bounds the post-spawn readiness poll. Non-positive
	// means the 3s default.
	WaitTimeout time.Duration
}

// StartResult reports what [Start] found or did.
type StartResult struct {
	// PID is the live daemon's process id (freshly spawned or already
	// running).
	PID int
	// SocketPath is the live daemon's unix socket.
	SocketPath string
	// DaemonVersion is the live daemon's stamped version.
	DaemonVersion string
	// LogPath is where the spawned daemon's stderr goes. Empty when no
	// spawn happened (already running).
	LogPath string
	// AlreadyRunning is true when no spawn happened because a live
	// daemon was already serving (the idempotent path), or when a
	// concurrent start won the race.
	AlreadyRunning bool
	// VersionMismatch is true when the live daemon's version differs
	// from SelfVersion (exact string inequality, the Probe rule).
	VersionMismatch bool
}

// Start launches a detached guild daemon and waits for it to become
// ready.
//
// The daemon is spawned in its own session (setsid) with stdin and
// stdout on the null device and stderr appended to the log file, so it
// survives the parent and the parent's terminal. The previous log
// generation is rotated to <log>.old first (rotate-on-start retention:
// two generations, no size cap inside a generation).
//
// Idempotent: when a probe finds a live daemon before spawning, Start
// reports it via AlreadyRunning instead of spawning a second one. On
// readiness timeout or early child exit, the returned error carries
// the tail of the daemon log.
func Start(opts StartOptions) (StartResult, error) {
	if !lifecycleSupported {
		return StartResult{}, ErrLifecycleUnsupported
	}
	if opts.SelfVersion == "" {
		opts.SelfVersion = "dev"
	}
	if opts.WaitTimeout <= 0 {
		opts.WaitTimeout = defaultStartWait
	}

	// Idempotency probe: a live daemon of any version means no spawn.
	// Probe itself clears stale daemon.json records, so reaching the
	// spawn below means the discovery path is clean.
	res, live, err := Probe(opts.SelfVersion, lifecycleProbeTimeout)
	if err != nil {
		return StartResult{}, err
	}
	if res != NotRunning {
		return liveStartResult(live, res, "", 0), nil
	}

	exe := opts.Exe
	if exe == "" {
		exe, err = os.Executable()
		if err != nil {
			return StartResult{}, fmt.Errorf("daemon: resolve own executable: %w", err)
		}
	}
	logPath := opts.LogPath
	if logPath == "" {
		logPath, err = LogPath()
		if err != nil {
			return StartResult{}, err
		}
	}
	rotateLog(logPath)
	// G304: logPath is ~/.guild/daemon.log or a test-supplied path.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode) //nolint:gosec // trusted path; see note above
	if err != nil {
		return StartResult{}, fmt.Errorf("daemon: open %s: %w", logPath, err)
	}

	args := opts.Args
	if args == nil {
		args = []string{"daemon", "run"}
	}
	// G204: exe is os.Executable() (or a test-supplied helper) and args
	// default to the fixed "daemon run" argv; nothing user-controlled.
	cmd := exec.Command(exe, args...) //nolint:gosec // re-exec of own binary; see note above
	cmd.Env = opts.Env
	// Stdin and Stdout stay nil: os/exec connects nil std streams to
	// the null device, which is exactly the detachment contract.
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachSysProcAttr()
	// Anchor the daemon's cwd at the user's home directory instead of
	// inheriting the operator's shell cwd: sessions resolve project
	// context from each shim's cwd, and the daemon must not pin a
	// removable or soon-deleted directory for its whole lifetime.
	if home, herr := os.UserHomeDir(); herr == nil {
		cmd.Dir = home
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return StartResult{}, fmt.Errorf("daemon: spawn %s: %w", exe, err)
	}
	// The child holds its own descriptor; the parent's copy closes now
	// so a wedged CLI never pins the log open.
	_ = logFile.Close()

	// Reap in the background. If the child dies during the readiness
	// poll it must not linger as a zombie: kill(pid, 0) reports zombies
	// as alive, which would wedge the probe loop until the deadline.
	// On success the goroutine blocks until this short-lived CLI
	// process exits, which is fine.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	deadline := time.Now().Add(opts.WaitTimeout)
	for {
		res, live, perr := Probe(opts.SelfVersion, lifecycleProbeTimeout)
		if perr == nil && res != NotRunning {
			return liveStartResult(live, res, logPath, cmd.Process.Pid), nil
		}
		select {
		case <-exited:
			// The child exited before becoming ready. One last probe
			// catches the race where it refused to start because a
			// concurrent start's daemon is in fact serving.
			if res, live, perr := Probe(opts.SelfVersion, lifecycleProbeTimeout); perr == nil && res != NotRunning {
				return liveStartResult(live, res, logPath, 0), nil
			}
			return StartResult{}, startFailure(logPath, "daemon process exited during startup")
		default:
		}
		if time.Now().After(deadline) {
			return StartResult{}, startFailure(logPath, fmt.Sprintf("daemon not ready within %s", opts.WaitTimeout))
		}
		time.Sleep(startPollInterval)
	}
}

// liveStartResult builds the StartResult for a live daemon found by a
// probe. spawnedPID is the pid Start itself spawned (0 when nothing
// was spawned); any other live pid means the daemon was already
// running or a concurrent start won the race.
func liveStartResult(live Discovery, res ProbeResult, logPath string, spawnedPID int) StartResult {
	return StartResult{
		PID:             live.PID,
		SocketPath:      live.SocketPath,
		DaemonVersion:   live.Version,
		LogPath:         logPath,
		AlreadyRunning:  live.PID != spawnedPID,
		VersionMismatch: res == RunningMismatch,
	}
}

// startFailure wraps a Start failure with the tail of the daemon log,
// so the operator sees the daemon's own error without hunting for the
// file.
func startFailure(logPath, reason string) error {
	tail := tailLog(logPath, logTailBytes)
	if tail == "" {
		return fmt.Errorf("daemon: start failed: %s (no output captured in %s)", reason, logPath)
	}
	return fmt.Errorf("daemon: start failed: %s; tail of %s:\n%s", reason, logPath, tail)
}

// StopOptions configures [Stop]. The zero value is production
// behavior: 5s SIGTERM grace, 2s SIGKILL verification.
type StopOptions struct {
	// SelfVersion is the invoking binary's stamped version, used only
	// for the liveness probe (Stop stops mismatched daemons too).
	// Empty defaults to "dev".
	SelfVersion string

	// TermTimeout is the grace period after SIGTERM before escalating
	// to SIGKILL. Non-positive means the 5s default.
	TermTimeout time.Duration

	// KillTimeout bounds the wait for the process to vanish after
	// SIGKILL. Non-positive means the 2s default.
	KillTimeout time.Duration
}

// StopResult reports what [Stop] found or did.
type StopResult struct {
	// WasRunning is false when no live daemon was found (the
	// idempotent no-op path).
	WasRunning bool
	// PID is the stopped daemon's process id (zero when !WasRunning).
	PID int
	// Killed is true when the daemon ignored SIGTERM past the grace
	// period and was escalated to SIGKILL.
	Killed bool
}

// Stop terminates the running guild daemon: SIGTERM, a bounded wait,
// then SIGKILL escalation. After the process is gone it removes the
// socket and discovery file on the daemon's behalf when the daemon
// could not run its own cleanup (the SIGKILL path), so both artifacts
// are verifiably gone on return.
//
// Idempotent: a missing or stale daemon.json reports WasRunning=false
// with a nil error.
func Stop(opts StopOptions) (StopResult, error) {
	if !lifecycleSupported {
		return StopResult{}, ErrLifecycleUnsupported
	}
	if opts.SelfVersion == "" {
		opts.SelfVersion = "dev"
	}
	if opts.TermTimeout <= 0 {
		opts.TermTimeout = defaultTermWait
	}
	if opts.KillTimeout <= 0 {
		opts.KillTimeout = defaultKillWait
	}

	// Probe (not a bare ReadDiscovery) so stale records are classified
	// and cleared exactly the way every other prober does it.
	res, live, err := Probe(opts.SelfVersion, lifecycleProbeTimeout)
	if err != nil {
		return StopResult{}, err
	}
	if res == NotRunning {
		return StopResult{}, nil
	}

	if err := terminateProcess(live.PID); err != nil {
		return StopResult{}, fmt.Errorf("daemon: signal pid %d: %w", live.PID, err)
	}

	killed := false
	if !waitProcessGone(live.PID, opts.TermTimeout) {
		killed = true
		if err := killProcess(live.PID); err != nil {
			return StopResult{}, fmt.Errorf("daemon: kill pid %d: %w", live.PID, err)
		}
		if !waitProcessGone(live.PID, opts.KillTimeout) {
			return StopResult{WasRunning: true, PID: live.PID, Killed: true},
				fmt.Errorf("daemon: pid %d still alive after SIGKILL", live.PID)
		}
	}

	cleanupAfterStop(live)
	return StopResult{WasRunning: true, PID: live.PID, Killed: killed}, nil
}

// waitProcessGone polls until pid no longer exists or timeout elapses.
// Reports true when the process is gone.
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := processAlive(pid)
		if err == nil && !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(stopPollInterval)
	}
}

// cleanupAfterStop removes the socket and discovery file the stopped
// daemon left behind when it could not run its own shutdown path
// (SIGKILL, crash mid-teardown). Guarded on pid: when a different
// daemon already wrote a fresh record (a restart race won by someone
// else), its artifacts are not ours to touch.
func cleanupAfterStop(stopped Discovery) {
	d, err := ReadDiscovery()
	if err == nil && d != nil && d.PID != stopped.PID {
		return
	}
	removeDiscovery()
	if stopped.SocketPath != "" {
		_ = os.Remove(stopped.SocketPath)
	}
}

// StatusReport is the lifecycle-level view of the daemon: the daemon's
// own [Status] reply plus fields the prober computes locally.
type StatusReport struct {
	// Running is false when no live daemon was found; every other
	// field except SelfVersion is zero in that case.
	Running bool
	// Status is the daemon's one-line reply to the status-request
	// preamble (pid, version, started_at, active sessions, embedder).
	Status Status
	// SocketPath is the live daemon's unix socket.
	SocketPath string
	// Uptime is now minus the daemon's started_at, truncated to whole
	// seconds.
	Uptime time.Duration
	// SelfVersion echoes the invoking binary's stamped version.
	SelfVersion string
	// Drift is true when the daemon's version differs from
	// SelfVersion (exact string inequality, the Probe rule).
	Drift bool
	// DriftNudge is the one-line operator nudge for Drift, empty when
	// the versions match. See [FormatVersionDrift].
	DriftNudge string
}

// QueryStatus probes for a live daemon and, when one is found, asks it
// for its status line over the socket ({"guild_status_request":{}}
// preamble). A clean "nothing is running" is (StatusReport{Running:
// false}, nil); the error return is reserved for environmental
// failures and a live daemon that fails to answer.
func QueryStatus(selfVersion string, timeout time.Duration) (StatusReport, error) {
	if !lifecycleSupported {
		return StatusReport{}, ErrLifecycleUnsupported
	}
	if selfVersion == "" {
		selfVersion = "dev"
	}
	if timeout <= 0 {
		timeout = lifecycleProbeTimeout
	}

	res, live, err := Probe(selfVersion, timeout)
	if err != nil {
		return StatusReport{}, err
	}
	if res == NotRunning {
		return StatusReport{SelfVersion: selfVersion}, nil
	}

	st, err := fetchStatus(live.SocketPath, timeout)
	if err != nil {
		return StatusReport{}, fmt.Errorf("daemon: query status of pid %d: %w", live.PID, err)
	}

	return StatusReport{
		Running:     true,
		Status:      *st,
		SocketPath:  live.SocketPath,
		Uptime:      time.Since(st.StartedAt).Truncate(time.Second),
		SelfVersion: selfVersion,
		Drift:       st.Version != selfVersion,
		DriftNudge:  FormatVersionDrift(st.Version, selfVersion),
	}, nil
}

// fetchStatus dials the daemon socket, sends the status-request
// preamble, and decodes the single JSON line reply.
func fetchStatus(socketPath string, timeout time.Duration) (*Status, error) {
	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socketPath, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if _, err := conn.Write([]byte(`{"guild_status_request":{}}` + "\n")); err != nil {
		return nil, fmt.Errorf("write status request: %w", err)
	}
	line, readErr := bufio.NewReader(conn).ReadBytes('\n')
	if len(line) == 0 {
		if readErr != nil {
			return nil, fmt.Errorf("read status reply: %w", readErr)
		}
		return nil, errors.New("empty status reply")
	}
	st := &Status{}
	if err := json.Unmarshal(line, st); err != nil {
		return nil, fmt.Errorf("parse status reply: %w", err)
	}
	return st, nil
}

// FormatVersionDrift renders the one-line nudge the lifecycle verbs
// print when the running daemon's version differs from the invoking
// binary's. The drift decision itself is exact string inequality (the
// Probe rule); release.IsNewer only phrases the direction. Returns ""
// when the versions match.
func FormatVersionDrift(daemonV, selfV string) string {
	if daemonV == selfV {
		return ""
	}
	if daemonNewer, _ := release.IsNewer(selfV, daemonV); daemonNewer {
		return fmt.Sprintf("version drift: daemon is %s, newer than this binary (%s); upgrade this binary, or run 'guild daemon restart' to match.", daemonV, selfV)
	}
	if selfNewer, _ := release.IsNewer(daemonV, selfV); selfNewer {
		return fmt.Sprintf("version drift: daemon is %s, older than this binary (%s); run 'guild daemon restart' to pick up the newer binary.", daemonV, selfV)
	}
	// Direction is indeterminate (dev builds and other non-semver
	// stamps land here).
	return fmt.Sprintf("version drift: daemon is %s, which does not match this binary (%s); run 'guild daemon restart' to clear the mismatch.", daemonV, selfV)
}

// rotateLog implements the v1 retention strategy: rotate-on-start. A
// non-empty log from the previous daemon generation is renamed to
// <path>.old (replacing the prior .old), so every start writes a fresh
// log while the previous generation stays inspectable. Total retention
// is two generations; a size cap inside one generation is deliberately
// out of scope for v1.
func rotateLog(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return
	}
	_ = os.Rename(path, path+".old")
}

// tailLog returns up to the last maxBytes bytes of the file at path,
// trimmed to whole lines. Empty string when the file is missing,
// empty, or unreadable: callers degrade to a tail-less error message.
func tailLog(path string, maxBytes int64) string {
	// G304: path is ~/.guild/daemon.log or a test-supplied path.
	f, err := os.Open(path) //nolint:gosec // trusted path; see note above
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return ""
	}
	off := int64(0)
	if fi.Size() > maxBytes {
		off = fi.Size() - maxBytes
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return ""
	}
	s := strings.TrimSpace(string(buf))
	if off > 0 {
		// The window almost certainly starts mid-line; drop the
		// partial first line when a complete one follows.
		if i := strings.IndexByte(s, '\n'); i >= 0 && i+1 < len(s) {
			s = s[i+1:]
		}
	}
	return s
}
