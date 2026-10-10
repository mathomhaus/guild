package watch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

// DefaultMaxWatchPaths bounds optional filesystem watching in a resident
// daemon. kqueue uses a descriptor per file, not just per directory.
const DefaultMaxWatchPaths = 4096

var errWatchBudget = errors.New("native watch path budget exceeded")

func resourceExhausted(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOSPC)
}

func kqueueBackend() bool {
	switch runtime.GOOS {
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		return true
	default:
		return false
	}
}

// Reserve kqueue's implicit file watches before touching the backend,
// including directory entries filtered out of our event stream.
func (w *Watcher) addWatch(path string) error {
	paths := []string{path}
	if kqueueBackend() {
		entries, err := os.ReadDir(path)
		if err != nil {
			if resourceExhausted(err) {
				w.watchErr = err
			}
			return err
		}
		for _, entry := range entries {
			paths = append(paths, filepath.Join(path, entry.Name()))
		}
	}
	newPaths := 0
	for _, p := range paths {
		if _, exists := w.nativePaths[p]; !exists {
			newPaths++
		}
	}
	if len(w.nativePaths)+newPaths > w.maxPaths {
		w.watchErr = fmt.Errorf("limit %d: %w", w.maxPaths, errWatchBudget)
		return w.watchErr
	}
	// A failed kqueue Add can leave partially registered native watches.
	// Keep reservations conservative until the entire backend closes.
	for _, p := range paths {
		w.nativePaths[p] = struct{}{}
	}
	err := w.fs.Add(path)
	if resourceExhausted(err) {
		w.watchErr = err
	}
	return err
}

func (w *Watcher) trackNativeEvent(raw fsnotify.Event) {
	path := filepath.Clean(raw.Name)
	if raw.Op.Has(fsnotify.Remove | fsnotify.Rename) {
		// Descendant watches may remain live after their parent is moved.
		// Release them only when their own native removal events arrive.
		delete(w.nativePaths, path)
	}
	if !kqueueBackend() || !raw.Op.Has(fsnotify.Create) {
		return
	}
	if _, exists := w.nativePaths[path]; exists {
		return
	}
	// fsnotify sends Create before implicitly opening the new file. Closing
	// the backend here also bounds growth after startup.
	if len(w.nativePaths) >= w.maxPaths {
		w.watchErr = fmt.Errorf("limit %d: %w", w.maxPaths, errWatchBudget)
		return
	}
	w.nativePaths[path] = struct{}{}
}
