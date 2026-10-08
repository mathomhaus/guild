package watch

// Git HEAD watching. The recursive walk in watch.go never descends
// into .git; this file registers the narrow slice of .git that
// reveals history movement and classifies events on it:
//
//   - <root>/.git itself (one non-recursive watch) so rewrites of
//     HEAD and packed-refs are visible. git updates both via
//     write-temp-then-rename, which surfaces as a Create on the
//     final name through the parent-directory watch.
//   - <root>/.git/refs and every directory below it, so loose ref
//     updates (commits, branch creation) are visible. New ref
//     directories (e.g. refs/heads/feature/ for a slashed branch
//     name) are registered as they appear.
//
// Everything else under .git (index, COMMIT_EDITMSG, ORIG_HEAD,
// reflogs, objects) is noise for staleness purposes and is dropped,
// as are git's *.lock staging files. All accepted git activity in a
// root collapses into a single normalized event
// {Project, root path, KindGitHead} per debounce window, so a commit
// emits exactly one event and a branch switch emits exactly one
// event.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// addGitWatch registers the git-specific watches for one root. A
// root that is not a git repository, or whose .git is a file
// (worktrees, submodules), is skipped quietly: such projects simply
// never produce KindGitHead events, and the consumer's query-time
// staleness check still covers them.
func (w *Watcher) addGitWatch(root Root) {
	gitDir := filepath.Join(root.Path, ".git")
	fi, err := os.Lstat(gitDir)
	if err != nil {
		return // not a git repo; common and fine
	}
	if !fi.IsDir() {
		w.log.Debug("watch: .git is not a directory; skipping git watch",
			"project", root.Project, "path", gitDir)
		return
	}
	if err := w.fs.Add(gitDir); err != nil {
		w.log.Warn("watch: cannot watch .git; skipping git watch",
			"project", root.Project, "path", gitDir, "err", err)
		return
	}
	w.addRefDirTree(filepath.Join(gitDir, "refs"))
}

// addRefDirTree registers a watch on dir and every directory below
// it. Unlike addDirTree it applies no ignore rules: a branch may
// legitimately be named vendor or node_modules, and its ref
// directory must still be watched. Errors degrade quietly.
func (w *Watcher) addRefDirTree(dir string) {
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			w.log.Warn("watch: skipping unreadable ref path", "path", path, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil //nolint:nilerr // degrade quietly: skip the entry, keep walking
		}
		if !d.IsDir() {
			return nil
		}
		if addErr := w.fs.Add(path); addErr != nil {
			w.log.Warn("watch: cannot watch ref directory; skipping", "path", path, "err", addErr)
			return fs.SkipDir
		}
		return nil
	})
	if walkErr != nil {
		w.log.Warn("watch: ref walk failed", "dir", dir, "err", walkErr)
	}
}

// gitDirContaining reports whether path lies inside root's .git
// directory, returning that .git path when it does.
func gitDirContaining(root Root, path string) (gitDir string, ok bool) {
	gitDir = filepath.Join(root.Path, ".git")
	if path == gitDir || strings.HasPrefix(path, gitDir+string(os.PathSeparator)) {
		return gitDir, true
	}
	return "", false
}

// handleGitRaw processes a raw event on a path inside root's .git:
// register newly created ref directories, then fold HEAD-relevant
// activity into the single per-root git_head pending event.
func (w *Watcher) handleGitRaw(root Root, gitDir, path string, op fsnotify.Op) {
	refsDir := filepath.Join(gitDir, "refs")
	underRefs := path == refsDir || strings.HasPrefix(path, refsDir+string(os.PathSeparator))

	if underRefs && op.Has(fsnotify.Create) {
		// A slashed branch name (refs/heads/feature/x) creates
		// intermediate directories that need their own watches.
		if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
			w.addRefDirTree(path)
		}
	}

	if !isGitHeadPath(gitDir, refsDir, path, underRefs) {
		return
	}
	w.touch(Event{Project: root.Project, Path: root.Path, Kind: KindGitHead}, op)
}

// isGitHeadPath reports whether activity at path signals history
// movement: HEAD itself, packed-refs, or anything under refs/ that
// is not a *.lock staging file.
func isGitHeadPath(gitDir, refsDir, path string, underRefs bool) bool {
	if strings.HasSuffix(path, ".lock") {
		return false
	}
	if path == filepath.Join(gitDir, "HEAD") || path == filepath.Join(gitDir, "packed-refs") {
		return true
	}
	return underRefs && path != refsDir
}
