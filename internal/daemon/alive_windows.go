//go:build windows

package daemon

import (
	"errors"
	"os"
)

// realProcessAlive (Windows) uses os.FindProcess + a best-effort
// signal-0 equivalent, mirroring internal/session/cleanup_windows.go
// (duplicated, not imported, to keep this package a leaf). Platform
// notes:
//
//   - os.FindProcess on Windows does a real OpenProcess under the
//     hood, so a nil error means the PID is valid at probe time.
//   - Process.Signal(syscall.Signal(0)) reports "not supported by
//     windows" even for live PIDs, so we cannot replicate the Unix
//     behavior precisely. A successful FindProcess is treated as
//     "alive".
//
// Known limitation: PID recycling can make a dead daemon look alive,
// in which case the socket dial in Probe is the backstop that still
// classifies it as stale. See cleanup_windows.go for the upgrade path
// (OpenProcess + GetExitCodeProcess STILL_ACTIVE) if a real Windows
// daemon user appears.
func realProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		// os.FindProcess never returns an error on Windows except for
		// OS-level failures; treat as "cannot probe" and report.
		return false, err
	}
	// Release the handle so we don't leak it across many probes.
	defer func() { _ = proc.Release() }()

	// Best-effort signal probe. We accept false positives (see limits
	// above) in exchange for a probe that doesn't shell out.
	if err := proc.Signal(os.Signal(nil)); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrProcessDone) {
		return false, nil
	}
	// Other errors are usually "not supported"; fall back to the
	// optimistic "the FindProcess succeeded, assume alive" answer.
	return true, nil
}
