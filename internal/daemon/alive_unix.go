//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"syscall"
)

// realProcessAlive (Unix) probes via signal 0. Semantics mirror
// internal/session/cleanup_unix.go (duplicated, not imported, to keep
// this package a leaf):
//
//   - nil error  -> process exists (our uid or signal-permitted).
//   - ESRCH      -> process does not exist.
//   - ENOENT     -> same as ESRCH on some BSDs.
//   - EPERM      -> process exists but not ours to signal; treat as
//     alive and leave the discovery file in place (it belongs to a
//     different user's guild install).
//
// Any other errno is returned as a real error so Probe can surface it
// instead of misclassifying a live daemon as stale.
func realProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, fmt.Errorf("daemon: probe pid %d: %w", pid, err)
}
