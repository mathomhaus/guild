//go:build unix

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// tryLockExclusive takes a non-blocking exclusive advisory lock on f.
// Reports won=true when this process now holds the lock, won=false when
// another process already holds it (EWOULDBLOCK/EAGAIN). The lock is
// released by [unlock] or when the descriptor closes (process exit), so
// a crashing spawner never wedges the next shim's election.
func tryLockExclusive(f *os.File) (won bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

// unlock releases the advisory lock held on f. Closing the descriptor
// would release it too; unlock is the explicit form the winner uses
// once its daemon is up so a later shim can re-probe under a free lock
// without waiting on the spawner's eventual exit.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
