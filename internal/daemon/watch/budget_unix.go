//go:build !windows

package watch

import "syscall"

func watchPathLimit(requested int) int {
	if requested <= 0 || requested > DefaultMaxWatchPaths {
		requested = DefaultMaxWatchPaths
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err == nil {
		available := uint64(limit.Cur) / 4
		if available == 0 {
			available = 1
		}
		if available <= DefaultMaxWatchPaths {
			bounded := int(available)
			if bounded < requested {
				requested = bounded
			}
		}
	}
	return requested
}
