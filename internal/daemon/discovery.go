// Package daemon provides the shared discovery primitives for the
// guild daemon: the ~/.guild/daemon.json discovery file, a liveness
// probe, and the version-skew rule every prober applies.
//
// This package is a deliberate leaf. internal/cli, internal/mcp, and
// cmd/guild will all import it, so it must never import either of the
// first two (import cycle) and stays dependency-light otherwise. The
// pid-alive idiom is duplicated from internal/session rather than
// imported for exactly that reason.
//
// Contract:
//
//   - The daemon writes daemon.json (pid, version, socket path,
//     started_at) on startup via WriteDiscovery.
//   - Every prober (stdio shim, CLI verbs, lifecycle verbs) calls
//     Probe with its own build-stamped version. A daemon is "running"
//     only if its pid is alive AND its unix socket accepts a
//     connection within the timeout; anything else is stale and the
//     stale daemon.json is removed best-effort.
//   - Version comparison is EXACT STRING EQUALITY, never semver. Dev
//     builds stamp "dev" (see cmd/guild/main.go ldflags vars) and skew
//     in either direction must fall back to in-process mode. Semver
//     ordering (release.IsNewer) is used only to phrase the direction
//     of the one-line nudge, never for the match decision.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/release"
)

// discoveryFileName is the basename of the discovery file under
// ~/.guild/.
const discoveryFileName = "daemon.json"

// discoveryFileMode is the mode bits for ~/.guild/daemon.json. The
// file carries nothing secret (pid, version, socket path) but is still
// per-user state, and the socket path it points at must not be
// advertised wider than the 0700 ~/.guild dir already allows.
const discoveryFileMode os.FileMode = 0o600

// ErrCorruptDiscovery wraps read failures where daemon.json exists but
// does not parse as JSON. Probe treats it as a stale file (removed
// best-effort); other callers can errors.Is against it to decide.
var ErrCorruptDiscovery = errors.New("daemon: corrupt discovery file")

// Discovery is the on-disk record the daemon writes to
// ~/.guild/daemon.json at startup. JSON keys are stable; additions are
// append-only so older binaries can still read newer files.
type Discovery struct {
	// PID is the daemon's process id.
	PID int `json:"pid"`
	// Version is the daemon's ldflags-stamped build version, e.g.
	// "v0.3.2" for releases or "dev" for plain go-build binaries.
	Version string `json:"version"`
	// SocketPath is the unix socket the daemon listens on.
	SocketPath string `json:"socket_path"`
	// StartedAt is the daemon's startup time, serialized RFC 3339.
	StartedAt time.Time `json:"started_at"`
}

// DiscoveryPath returns the discovery file location
// (~/.guild/daemon.json), ensuring ~/.guild exists with 0700 first.
func DiscoveryPath() (string, error) {
	dir, err := guildpath.EnsureGuildDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve discovery path: %w", err)
	}
	return filepath.Join(dir, discoveryFileName), nil
}

// WriteDiscovery atomically writes d to ~/.guild/daemon.json: the
// record lands in a unique 0600 temp sibling first, then renames over
// the final path. A crash between write and rename leaves a stray
// .tmp file but never a partial daemon.json a concurrent prober could
// half-read.
func WriteDiscovery(d Discovery) error {
	final, err := DiscoveryPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: marshal discovery: %w", err)
	}
	data = append(data, '\n')

	// os.CreateTemp opens with O_EXCL and 0600, so concurrent writers
	// get distinct temp files and the perms are right from creation
	// (no chmod window).
	f, err := os.CreateTemp(filepath.Dir(final), discoveryFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("daemon: create temp discovery file: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("daemon: write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("daemon: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("daemon: rename %s -> %s: %w", tmp, final, err)
	}
	return nil
}

// ReadDiscovery reads and decodes ~/.guild/daemon.json. A missing (or
// empty) file returns (nil, nil): "no daemon has ever started" is the
// common case, not an error. A file that exists but does not parse
// returns an error wrapping ErrCorruptDiscovery.
func ReadDiscovery() (*Discovery, error) {
	path, err := DiscoveryPath()
	if err != nil {
		return nil, err
	}
	// G304: path is built from the trusted ~/.guild dir plus a
	// compile-time constant basename; no user-controlled input.
	data, err := os.ReadFile(path) //nolint:gosec // trusted path; see note above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("daemon: read %s: %w", path, err)
	}
	if len(data) == 0 {
		// Treat an empty file like a missing one; the atomic rename in
		// WriteDiscovery never produces one, so this is tampering or a
		// filesystem fault, and "not running" is the safe answer.
		return nil, nil
	}
	d := &Discovery{}
	if err := json.Unmarshal(data, d); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", ErrCorruptDiscovery, path, err)
	}
	return d, nil
}

// removeDiscovery deletes daemon.json best-effort. Called when a probe
// finds the record stale (dead pid, undialable socket, corrupt JSON).
// A concurrent daemon restart can theoretically rewrite the file
// between our read and this remove; that daemon will simply rewrite
// its record on the next startup pass, so losing the race is benign.
func removeDiscovery() {
	path, err := DiscoveryPath()
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

// ProbeResult is the three-way outcome of Probe.
type ProbeResult int

const (
	// NotRunning means no live daemon was found: the discovery file is
	// missing, corrupt, or stale (dead pid or undialable socket).
	NotRunning ProbeResult = iota
	// RunningMatch means a live daemon was found and its stamped
	// version is byte-for-byte equal to the caller's.
	RunningMatch
	// RunningMismatch means a live daemon was found but its stamped
	// version differs from the caller's in any way, including dev
	// builds and skew in either direction. Callers fall back to
	// in-process mode and print FormatSkewNudge once.
	RunningMismatch
)

// String renders the snake_case names used in logs and acceptance
// criteria: not_running, running_match, running_mismatch.
func (r ProbeResult) String() string {
	switch r {
	case NotRunning:
		return "not_running"
	case RunningMatch:
		return "running_match"
	case RunningMismatch:
		return "running_mismatch"
	default:
		return fmt.Sprintf("ProbeResult(%d)", int(r))
	}
}

// Probe checks whether a live guild daemon is reachable and whether
// its version matches selfVersion (the caller's own ldflags-stamped
// version).
//
// Liveness requires BOTH: the recorded pid is alive AND the recorded
// unix socket accepts a connection within timeout (a non-positive
// timeout dials without a deadline). A discovery file that fails
// either check is stale and removed best-effort.
//
// On RunningMatch and RunningMismatch the returned Discovery carries
// the daemon's version and socket path; on NotRunning it is the zero
// value. The error return is reserved for environmental failures
// (unresolvable home, unreadable file, probe errno): a clean "nothing
// is running" is (NotRunning, Discovery{}, nil).
func Probe(selfVersion string, timeout time.Duration) (ProbeResult, Discovery, error) {
	d, err := ReadDiscovery()
	if err != nil {
		if errors.Is(err, ErrCorruptDiscovery) {
			removeDiscovery()
			return NotRunning, Discovery{}, nil
		}
		return NotRunning, Discovery{}, err
	}
	if d == nil {
		return NotRunning, Discovery{}, nil
	}

	alive, err := processAlive(d.PID)
	if err != nil {
		return NotRunning, Discovery{}, fmt.Errorf("daemon: probe pid %d: %w", d.PID, err)
	}
	if !alive {
		removeDiscovery()
		return NotRunning, Discovery{}, nil
	}

	conn, err := net.DialTimeout("unix", d.SocketPath, timeout)
	if err != nil {
		// Pid is alive but the socket is gone or wedged: treat as not
		// running so callers fall back in-process rather than hanging.
		removeDiscovery()
		return NotRunning, Discovery{}, nil
	}
	_ = conn.Close()

	if d.Version == selfVersion {
		return RunningMatch, *d, nil
	}
	return RunningMismatch, *d, nil
}

// FormatSkewNudge renders the single nudge line every prober prints on
// RunningMismatch, so the stdio shim and the CLI emit identical text.
// release.IsNewer is used only to phrase the direction (newer vs
// older); the mismatch decision itself is exact string equality in
// Probe. Returns "" if the versions are equal (no skew to report).
func FormatSkewNudge(daemonV, selfV string) string {
	if daemonV == selfV {
		return ""
	}
	if daemonNewer, _ := release.IsNewer(selfV, daemonV); daemonNewer {
		return fmt.Sprintf("guild daemon is running %s, newer than this binary (%s); continuing in-process. Upgrade this binary, or restart the daemon to match.", daemonV, selfV)
	}
	if selfNewer, _ := release.IsNewer(daemonV, selfV); selfNewer {
		return fmt.Sprintf("guild daemon is running %s, older than this binary (%s); continuing in-process. Restart the daemon to pick up the newer binary.", daemonV, selfV)
	}
	// Direction is indeterminate (dev builds and other non-semver
	// stamps land here).
	return fmt.Sprintf("guild daemon is running %s, which does not match this binary (%s); continuing in-process. Restart the daemon to clear the mismatch.", daemonV, selfV)
}

// processAlive reports whether a process with the given pid exists.
// Split out so tests can swap in a fake probe via processAliveFn.
func processAlive(pid int) (bool, error) { return processAliveFn(pid) }

// processAliveFn is the indirection test code overrides. Swap it via
// swapProcessAlive() in a test and restore in a t.Cleanup hook.
var processAliveFn = realProcessAlive

// swapProcessAlive replaces processAliveFn with fn and returns a
// restore closure. Test-only; package-private.
func swapProcessAlive(fn func(int) (bool, error)) (restore func()) {
	prev := processAliveFn
	processAliveFn = fn
	return func() { processAliveFn = prev }
}

// realProcessAlive is provided per-platform:
//
//   - Unix (darwin, linux, BSDs): alive_unix.go implements it via
//     syscall.Kill(pid, 0) + errno ESRCH/EPERM handling, mirroring
//     internal/session/cleanup_unix.go (duplicated, not imported, to
//     keep this package a leaf).
//   - Windows: alive_windows.go implements it via os.FindProcess +
//     best-effort probe, mirroring internal/session/cleanup_windows.go.
