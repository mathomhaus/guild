//go:build windows

package daemon

import "os"

// tryLockExclusive (windows) is never reached: Autostart gates on
// lifecycleSupported (false on windows) and returns
// ErrLifecycleUnsupported before touching the lock. It exists so the
// shared autostart code compiles on windows.
func tryLockExclusive(*os.File) (won bool, err error) {
	return false, ErrLifecycleUnsupported
}

// unlock (windows) is never reached, for the same reason as
// tryLockExclusive.
func unlock(*os.File) error { return ErrLifecycleUnsupported }
