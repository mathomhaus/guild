package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mathomhaus/guild/internal/guildpath"
)

// This file implements ADR-005's autostart: the first stdio shim that
// finds no running daemon spawns one, under an exclusive file lock so
// that N shims racing on a cold ~/.guild elect exactly one spawner
// (the ollama pattern). Everyone else waits for the winner's daemon and
// then pipes to it.
//
// The decision to autostart lives with the shim, never the discovery
// prober: discovery stays a passive prober (QUEST-337). This helper is
// the spawn arm the shim host reaches for after Probe returns
// NotRunning AND config.Daemon.Autostart is true. When autostart is
// disabled the host must NOT call this at all, so the no-daemon path
// creates no lock file and attempts no spawn (the hard invariant: the
// opt-out path is byte-identical to a build without daemon support).
//
// Platform note: autostart is unix-only, gated on lifecycleSupported
// the same way Start is. The non-unix stub (autostart_other.go) reports
// ErrLifecycleUnsupported so the host falls straight through to the
// in-process server.

// lockFileName is the basename of the autostart election lock under
// ~/.guild/. The lock guards the spawn decision only; the daemon's own
// single-instance guarantee still comes from the listen-side
// ErrAlreadyRunning check in NewServer, so a stale lock can never cause
// a second daemon to bind the socket.
const lockFileName = "daemon.lock"

// lockFileMode is the mode bits for ~/.guild/daemon.lock. The file is
// empty (its only role is to be flock'd) but still gets the per-user
// 0600 treatment every artifact inside the 0700 dir gets.
const lockFileMode os.FileMode = 0o600

// autostartPollInterval paces the loser's wait for the winner's daemon
// to come up, and the winner's own post-spawn readiness re-probe.
const autostartPollInterval = 25 * time.Millisecond

// defaultAutostartWait bounds the whole autostart attempt: a winner's
// spawn-and-readiness loop, or a loser's wait for the winner. It tracks
// the lifecycle Start budget (~3s) so a wedged spawn never pins an
// exiting harness; on deadline the helper returns NotRunning and the
// host falls back in-process.
const defaultAutostartWait = 3 * time.Second

// LockPath returns the autostart lock file location
// (~/.guild/daemon.lock), ensuring ~/.guild exists with 0700 first.
func LockPath() (string, error) {
	dir, err := guildpath.EnsureGuildDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve lock path: %w", err)
	}
	return filepath.Join(dir, lockFileName), nil
}

// AutostartOptions configures [Autostart]. The zero value is production
// behavior: re-exec the current binary with "daemon run", inherit the
// environment, ~3s overall budget. The Exe/Args/Env/LogPath/WaitTimeout
// overrides exist for tests, which substitute a helper process for the
// real binary; they are threaded straight into the lifecycle [Start].
type AutostartOptions struct {
	// SelfVersion is the invoking binary's ldflags-stamped version, used
	// both for the readiness re-probe (a match is what lets the shim
	// pipe) and stamped into the spawned daemon's environment. Empty
	// defaults to "dev".
	SelfVersion string

	// Exe overrides the program spawned for the daemon. Empty means
	// os.Executable(). Forwarded to [StartOptions.Exe].
	Exe string

	// Args overrides the spawned program's argv. nil means the
	// production ["daemon", "run"]. Forwarded to [StartOptions.Args].
	Args []string

	// Env overrides the spawned daemon's environment. nil inherits the
	// parent's. Forwarded to [StartOptions.Env].
	Env []string

	// LogPath overrides where the spawned daemon's stderr lands. Empty
	// means ~/.guild/daemon.log. Forwarded to [StartOptions.LogPath].
	LogPath string

	// Wait bounds the whole attempt (winner spawn + readiness, or loser
	// wait). Non-positive means the ~3s default.
	Wait time.Duration
}

// Autostart spawns a guild daemon when none is running, electing a
// single spawner across concurrent callers via an exclusive lock on
// ~/.guild/daemon.lock.
//
// Caller contract: invoke ONLY after a probe returned NotRunning AND
// autostart is enabled in config. The result is the probe outcome the
// caller should then act on:
//
//   - RunningMatch: a daemon (this call's spawn, or a concurrent
//     winner's) is up and version-matched; the caller pipes to
//     live.SocketPath.
//   - RunningMismatch: a daemon is up but version-skewed; the caller
//     nudges and serves in-process (never piped, never re-spawned).
//   - NotRunning: no daemon came up within the budget; the caller
//     serves in-process. This is also the all-failures answer.
//
// The error return is reserved for environmental faults (unresolvable
// home, unopenable lock file). On any error the caller serves
// in-process: correctness never depends on the daemon. Autostart never
// returns ErrLifecycleUnsupported as a hard error on unix; the non-unix
// stub returns it so cross-platform callers can detect "no daemon
// here" without a platform check.
func Autostart(opts AutostartOptions) (ProbeResult, Discovery, error) {
	if !lifecycleSupported {
		return NotRunning, Discovery{}, ErrLifecycleUnsupported
	}
	if opts.SelfVersion == "" {
		opts.SelfVersion = "dev"
	}
	wait := opts.Wait
	if wait <= 0 {
		wait = defaultAutostartWait
	}
	deadline := time.Now().Add(wait)

	lockPath, err := LockPath()
	if err != nil {
		return NotRunning, Discovery{}, err
	}
	// G304: lockPath is ~/.guild/daemon.lock, a trusted constant
	// basename under the per-user 0700 dir; no user-controlled input.
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, lockFileMode) //nolint:gosec // trusted path; see note above
	if err != nil {
		return NotRunning, Discovery{}, fmt.Errorf("daemon: open lock %s: %w", lockPath, err)
	}
	defer func() { _ = lockFile.Close() }()

	won, err := tryLockExclusive(lockFile)
	if err != nil {
		return NotRunning, Discovery{}, fmt.Errorf("daemon: lock %s: %w", lockPath, err)
	}
	if !won {
		// Someone else is spawning. Wait for their daemon rather than
		// piling on a second spawn (thundering-herd guard).
		return waitForDaemon(opts.SelfVersion, deadline)
	}
	// Hold the lock across the whole spawn + readiness window so a later
	// shim that grabs it sees a finished daemon on its re-probe.
	defer func() { _ = unlock(lockFile) }()

	// Double-check under the lock: a winner that finished spawning
	// between our first probe and acquiring this lock may already be
	// serving. Re-probe before spawning a redundant daemon.
	if res, live, perr := Probe(opts.SelfVersion, lifecycleProbeTimeout); perr == nil && res != NotRunning {
		return res, live, nil
	}

	startWait := time.Until(deadline)
	if startWait <= 0 {
		return NotRunning, Discovery{}, nil
	}
	res, err := Start(StartOptions{
		SelfVersion: opts.SelfVersion,
		Exe:         opts.Exe,
		Args:        opts.Args,
		Env:         opts.Env,
		LogPath:     opts.LogPath,
		WaitTimeout: startWait,
	})
	if err != nil {
		// Spawn or readiness failed. Surface the error so the caller can
		// log one diagnostic line, but pair it with NotRunning so the
		// in-process fallback is taken cleanly: correctness never depends
		// on the daemon.
		return NotRunning, Discovery{}, fmt.Errorf("daemon: autostart spawn: %w", err)
	}
	if res.VersionMismatch {
		return RunningMismatch, Discovery{
			PID:        res.PID,
			Version:    res.DaemonVersion,
			SocketPath: res.SocketPath,
		}, nil
	}
	return RunningMatch, Discovery{
		PID:        res.PID,
		Version:    res.DaemonVersion,
		SocketPath: res.SocketPath,
	}, nil
}

// waitForDaemon polls the probe until a daemon is running or the
// deadline passes. It is the loser-of-the-lock path: another shim holds
// the spawn lock, so this one simply waits for that daemon to appear
// rather than spawning its own. A clean "nobody came up in time" is
// (NotRunning, Discovery{}, nil) so the host falls back in-process.
func waitForDaemon(selfVersion string, deadline time.Time) (ProbeResult, Discovery, error) {
	for {
		res, live, err := Probe(selfVersion, lifecycleProbeTimeout)
		if err == nil && res != NotRunning {
			return res, live, nil
		}
		if time.Now().After(deadline) {
			return NotRunning, Discovery{}, nil
		}
		time.Sleep(autostartPollInterval)
	}
}
