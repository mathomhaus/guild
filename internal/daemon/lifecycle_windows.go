//go:build windows

package daemon

import "syscall"

// lifecycleSupported gates the lifecycle entry points per platform.
// False on windows: there is no daemon transport there yet, so Start,
// Stop, and QueryStatus return [ErrLifecycleUnsupported] and the
// no-daemon path stays the windows path. The package itself still
// compiles so the CLI surface and probers cross-build cleanly.
const lifecycleSupported = false

// detachSysProcAttr (windows) is never reached: Start gates on
// lifecycleSupported before spawning. It exists so the shared spawn
// code compiles.
func detachSysProcAttr() *syscall.SysProcAttr { return nil }

// terminateProcess (windows) is never reached: Stop gates on
// lifecycleSupported first.
func terminateProcess(int) error { return ErrLifecycleUnsupported }

// killProcess (windows) is never reached: Stop gates on
// lifecycleSupported first.
func killProcess(int) error { return ErrLifecycleUnsupported }
