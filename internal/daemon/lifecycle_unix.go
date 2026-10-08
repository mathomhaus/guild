//go:build unix

package daemon

import (
	"errors"
	"syscall"
)

// lifecycleSupported gates the lifecycle entry points per platform.
// True on unix: detached spawn, signals, and the unix socket all work.
const lifecycleSupported = true

// detachSysProcAttr returns the spawn attributes that detach the
// daemon into its own session (setsid): it survives the parent's exit
// and never holds the controlling terminal, so a closing shell cannot
// SIGHUP it.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateProcess sends SIGTERM to pid: the daemon's clean-shutdown
// signal (signal.NotifyContext in cmd/guild/daemon.go cancels Run's
// ctx, which removes the socket and discovery file before exit).
// ESRCH maps to nil: the process dying between probe and signal is the
// outcome Stop wanted.
func terminateProcess(pid int) error {
	err := syscall.Kill(pid, syscall.SIGTERM)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// killProcess sends SIGKILL to pid, the escalation when SIGTERM's
// grace period elapses. Same ESRCH-is-success rule as
// terminateProcess.
func killProcess(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
