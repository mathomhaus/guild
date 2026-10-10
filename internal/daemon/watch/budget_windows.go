//go:build windows

package watch

func watchPathLimit(requested int) int {
	if requested > 0 && requested <= DefaultMaxWatchPaths {
		return requested
	}
	return DefaultMaxWatchPaths
}
