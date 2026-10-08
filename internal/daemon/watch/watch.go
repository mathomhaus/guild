// Package watch turns filesystem activity under registered project
// roots into a debounced stream of normalized events. It is the
// event-driven half of lore staleness: instead of computing "what
// changed" at query time, a consumer subscribes to Events() and learns
// within seconds that a file was touched or a repo's HEAD moved.
//
// This package is a deliberate leaf: it imports fsnotify and the
// standard library only, and nothing else in the module imports it
// yet. Wiring it into the daemon loop is a separate change.
//
// Contract:
//
//   - New takes a set of {Project, Path} roots and starts watching
//     each root recursively. fsnotify watches single directories, so
//     the watcher walks each tree at startup and registers newly
//     created directories as they appear.
//   - Ignore rules (.guild, node_modules, vendor, .git internals) are
//     applied before watch registration, never after, so big
//     dependency trees cost zero OS watches.
//   - Raw fsnotify events are debounced per normalized event: a quiet
//     window (Options.Debounce, default 1s) must elapse after the
//     last raw event before the normalized Event is emitted. Editor
//     atomic-save bursts (write temp + rename over target) coalesce
//     to a single event for the target path.
//   - Git activity is summarized as Kind=git_head with Path set to
//     the project root: HEAD rewrites (branch switch), loose ref
//     updates (commit), and packed-refs rewrites all collapse into
//     one event per debounce window.
//   - Failures degrade quietly: unreadable directories, vanished
//     roots, and backend errors are logged and skipped, matching the
//     best-effort posture of the query-time staleness check this
//     stream augments. A dropped or missed event is cheap because
//     consumers re-derive truth from git and the database.
package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Kind classifies a normalized event.
type Kind string

const (
	// KindFile marks ordinary file or directory activity under a
	// project root.
	KindFile Kind = "file"
	// KindGitHead marks git history movement in a project root: a
	// commit, branch switch, ref update, or refs repack.
	KindGitHead Kind = "git_head"
)

// DefaultDebounce is the quiet window applied when Options.Debounce
// is not positive.
const DefaultDebounce = time.Second

// eventBuffer is the capacity of the outbound Events() channel. When
// a consumer falls this far behind, further events are dropped with a
// warning rather than stalling the watch loop; consumers re-derive
// truth on their next pass, so a dropped event only degrades to the
// query-time staleness check.
const eventBuffer = 256

// Root pairs a project identifier with the absolute path of its
// working tree.
type Root struct {
	// Project is the opaque identifier the consumer uses to map
	// events back to a registered project.
	Project string
	// Path is the absolute path of the project root.
	Path string
}

// Event is one normalized, debounced observation.
type Event struct {
	// Project is the Root.Project of the root containing the change.
	Project string
	// Path is the absolute path that changed for KindFile, or the
	// project root for KindGitHead (git activity is summarized per
	// repo, not per ref file).
	Path string
	// Kind classifies the change.
	Kind Kind
}

// Options tunes a Watcher. The zero value is usable.
type Options struct {
	// Debounce is the quiet window per normalized event: emission
	// waits until this long after the last raw filesystem event for
	// the same {Project, Path, Kind}. Non-positive means
	// DefaultDebounce.
	Debounce time.Duration
	// Logger receives degradation warnings (unreadable directories,
	// dropped events, backend errors). Nil falls back to
	// slog.Default(). Never logs to stdout unless that logger does.
	Logger *slog.Logger
}

// pending tracks one normalized event waiting out its debounce
// window.
type pending struct {
	// deadline is when the event may be emitted, pushed forward by
	// every new raw event for the same key.
	deadline time.Time
	// created records that the path appeared within this window. If
	// the path is then removed or renamed away before the deadline,
	// the whole entry is dropped as ephemeral (editor atomic-save
	// temp files, lockfiles). Only meaningful for KindFile.
	created bool
}

// Watcher owns one fsnotify watcher across all registered roots and
// the goroutine that normalizes, debounces, and emits events.
type Watcher struct {
	roots    []Root // sorted by descending path length for longest-prefix match
	debounce time.Duration
	log      *slog.Logger

	fs  *fsnotify.Watcher
	out chan Event

	// pendingEvents is touched only by the run goroutine.
	pendingEvents map[Event]*pending

	done      chan struct{} // closed by Close to stop run
	loopDone  chan struct{} // closed by run on exit
	closeOnce sync.Once
	closeErr  error
}

// New starts watching the given roots and returns the running
// Watcher. Every Root.Path must be absolute; that is the only
// per-root hard failure. A root that is missing or unreadable is
// logged and skipped so one stale registration cannot prevent the
// rest from being watched.
func New(roots []Root, opts Options) (*Watcher, error) {
	for _, r := range roots {
		if !filepath.IsAbs(r.Path) {
			return nil, fmt.Errorf("watch: root path %q for project %q is not absolute", r.Path, r.Project)
		}
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch: create fsnotify watcher: %w", err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = DefaultDebounce
	}

	w := &Watcher{
		roots:         make([]Root, 0, len(roots)),
		debounce:      debounce,
		log:           logger,
		fs:            fsw,
		out:           make(chan Event, eventBuffer),
		pendingEvents: make(map[Event]*pending),
		done:          make(chan struct{}),
		loopDone:      make(chan struct{}),
	}
	for _, r := range roots {
		w.roots = append(w.roots, Root{Project: r.Project, Path: filepath.Clean(r.Path)})
	}
	// Longest path first so nested roots resolve to the innermost
	// project.
	sort.Slice(w.roots, func(i, j int) bool {
		return len(w.roots[i].Path) > len(w.roots[j].Path)
	})

	for _, r := range w.roots {
		w.addDirTree(r.Path)
		w.addGitWatch(r)
	}

	go w.run()
	return w, nil
}

// Events returns the stream of normalized events. The channel is
// closed after Close.
func (w *Watcher) Events() <-chan Event { return w.out }

// Close releases all OS watches and closes the Events channel. Safe
// to call more than once.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
		w.closeErr = w.fs.Close()
		<-w.loopDone
	})
	return w.closeErr
}

// ignoredDirs are directory names skipped before watch registration
// anywhere under a root. .git is in the set because the recursive
// walk must never descend into it; the dedicated git watch in git.go
// registers the few .git paths that matter.
var ignoredDirs = map[string]bool{
	".guild":       true,
	"node_modules": true,
	"vendor":       true,
	".git":         true,
}

// run is the single goroutine that consumes raw fsnotify events,
// normalizes and debounces them, and emits to out. pendingEvents is
// owned exclusively by this goroutine.
func (w *Watcher) run() {
	defer close(w.loopDone)
	defer close(w.out)

	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-w.done:
			return
		case raw, ok := <-w.fs.Events:
			if !ok {
				w.flush(time.Time{}) // emit everything still pending
				return
			}
			w.handleRaw(raw)
			w.rescheduleFlush(timer)
		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			w.log.Warn("watch: backend error; continuing", "err", err)
		case now := <-timer.C:
			w.flush(now)
			w.rescheduleFlush(timer)
		}
	}
}

// handleRaw normalizes one raw fsnotify event: resolve the owning
// root, route git paths to the git classifier, apply ignore rules,
// register newly created directories, and fold the result into the
// pending set.
func (w *Watcher) handleRaw(raw fsnotify.Event) {
	path := filepath.Clean(raw.Name)
	root, ok := w.rootFor(path)
	if !ok {
		return
	}

	if gitDir, inGit := gitDirContaining(root, path); inGit {
		w.handleGitRaw(root, gitDir, path, raw.Op)
		return
	}

	rel, err := filepath.Rel(root.Path, path)
	if err != nil || hasIgnoredComponent(rel) {
		return
	}

	if raw.Op.Has(fsnotify.Create) {
		// fsnotify watches single directories, so a directory created
		// (or renamed in) after startup must be registered explicitly,
		// subtree and all.
		if fi, statErr := os.Lstat(path); statErr == nil && fi.IsDir() {
			w.addDirTree(path)
		}
	}

	w.touch(Event{Project: root.Project, Path: path, Kind: KindFile}, raw.Op)
}

// touch folds one raw operation into the pending set for the given
// normalized event, applying the ephemeral-file rule for KindFile:
// a path that was created and then removed or renamed away within
// one debounce window never existed as far as consumers are
// concerned. That is exactly the lifecycle of editor atomic-save
// temp files, so a write+rename burst emits one event for the saved
// path and none for the temp sibling.
func (w *Watcher) touch(key Event, op fsnotify.Op) {
	entry := w.pendingEvents[key]
	if key.Kind == KindFile && op.Has(fsnotify.Remove|fsnotify.Rename) {
		if entry != nil && entry.created {
			delete(w.pendingEvents, key)
			return
		}
	}
	if entry == nil {
		entry = &pending{}
		w.pendingEvents[key] = entry
	}
	if key.Kind == KindFile && op.Has(fsnotify.Create) {
		entry.created = true
	}
	entry.deadline = time.Now().Add(w.debounce)
}

// flush emits every pending event whose debounce deadline has
// passed. The zero time means "emit everything" (shutdown path).
func (w *Watcher) flush(now time.Time) {
	for key, entry := range w.pendingEvents {
		if now.IsZero() || !entry.deadline.After(now) {
			delete(w.pendingEvents, key)
			w.emit(key)
		}
	}
}

// rescheduleFlush re-arms the flush timer for the earliest pending
// deadline, or leaves it stopped when nothing is pending.
func (w *Watcher) rescheduleFlush(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	var earliest time.Time
	for _, entry := range w.pendingEvents {
		if earliest.IsZero() || entry.deadline.Before(earliest) {
			earliest = entry.deadline
		}
	}
	if earliest.IsZero() {
		return
	}
	d := time.Until(earliest)
	if d < 0 {
		d = 0
	}
	timer.Reset(d)
}

// emit hands one normalized event to the consumer without ever
// blocking the watch loop. A full channel drops the event with a
// warning; see eventBuffer for why that is acceptable.
func (w *Watcher) emit(ev Event) {
	select {
	case w.out <- ev:
	default:
		w.log.Warn("watch: event channel full, dropping event",
			"project", ev.Project, "path", ev.Path, "kind", string(ev.Kind))
	}
}

// rootFor resolves the registered root containing path. Roots are
// pre-sorted longest-first, so nested roots win over their parents.
func (w *Watcher) rootFor(path string) (Root, bool) {
	for _, r := range w.roots {
		if path == r.Path || strings.HasPrefix(path, r.Path+string(os.PathSeparator)) {
			return r, true
		}
	}
	return Root{}, false
}

// hasIgnoredComponent reports whether any component of the
// root-relative path rel is an ignored directory name.
func hasIgnoredComponent(rel string) bool {
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if ignoredDirs[part] {
			return true
		}
	}
	return false
}

// addDirTree walks the tree at dir and registers a watch on every
// directory that survives the ignore rules. Errors never propagate:
// an unreadable subdirectory is logged and skipped so the rest of
// the tree still gets watched.
func (w *Watcher) addDirTree(dir string) {
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			w.log.Warn("watch: skipping unreadable path", "path", path, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil //nolint:nilerr // degrade quietly: skip the entry, keep walking
		}
		if !d.IsDir() {
			return nil
		}
		if path != dir && ignoredDirs[d.Name()] {
			return fs.SkipDir
		}
		if addErr := w.fs.Add(path); addErr != nil {
			w.log.Warn("watch: cannot watch directory; skipping", "path", path, "err", addErr)
			return fs.SkipDir
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		w.log.Warn("watch: walk failed", "dir", dir, "err", walkErr)
	}
}
