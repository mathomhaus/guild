package watch_test

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/daemon/watch"
)

// testDebounce is short enough to keep the suite fast but comfortably
// longer than the per-event delivery latency of either backend
// (inotify, kqueue), so a single logical change still coalesces.
const testDebounce = 120 * time.Millisecond

// quietWindow is how long collect waits for silence before deciding
// the debounced emission is complete. It must exceed testDebounce so
// the first emitted event is always captured.
const quietWindow = 5 * testDebounce

// armDelay gives fsnotify a moment to finish registering watches
// before the test starts mutating the tree.
const armDelay = 60 * time.Millisecond

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newWatcher starts a watcher with the test debounce and a discarded
// logger, registers cleanup, and waits for watches to arm.
func newWatcher(t *testing.T, roots []watch.Root) *watch.Watcher {
	t.Helper()
	w, err := watch.New(roots, watch.Options{
		Debounce: testDebounce,
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	time.Sleep(armDelay)
	return w
}

// collect drains events until the stream is quiet for quietWindow,
// resetting the silence timer on each event so a burst is gathered in
// full.
func collect(t *testing.T, w *watch.Watcher) []watch.Event {
	t.Helper()
	var got []watch.Event
	timer := time.NewTimer(quietWindow)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quietWindow)
		case <-timer.C:
			return got
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestNewRejectsRelativeRoot(t *testing.T) {
	_, err := watch.New([]watch.Root{{Project: "p", Path: "relative/path"}}, watch.Options{})
	if err == nil {
		t.Fatal("expected error for relative root path, got nil")
	}
}

func TestFileCreateEmitsFileEvent(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(t, []watch.Root{{Project: "proj", Path: root}})

	target := filepath.Join(root, "note.txt")
	writeFile(t, target, "hello")

	got := collect(t, w)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(got), got)
	}
	if got[0].Kind != watch.KindFile {
		t.Errorf("kind = %q, want %q", got[0].Kind, watch.KindFile)
	}
	if got[0].Project != "proj" {
		t.Errorf("project = %q, want proj", got[0].Project)
	}
	if got[0].Path != target {
		t.Errorf("path = %q, want %q", got[0].Path, target)
	}
}

func TestDebounceCoalescesRepeatedWrites(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(t, []watch.Root{{Project: "proj", Path: root}})

	target := filepath.Join(root, "busy.txt")
	for i := 0; i < 5; i++ {
		writeFile(t, target, "content")
		time.Sleep(testDebounce / 4) // stay inside the sliding window
	}

	got := collect(t, w)
	if len(got) != 1 {
		t.Fatalf("want 1 coalesced event, got %d: %+v", len(got), got)
	}
	if got[0].Path != target || got[0].Kind != watch.KindFile {
		t.Errorf("event = %+v, want file event for %q", got[0], target)
	}
}

func TestAtomicRenameCoalescesToTarget(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(t, []watch.Root{{Project: "proj", Path: root}})

	// Editor atomic save: write a temp sibling, then rename it over
	// the target. The temp file is ephemeral and must not surface;
	// the target gets exactly one event.
	tmp := filepath.Join(root, "doc.txt.tmp")
	target := filepath.Join(root, "doc.txt")
	writeFile(t, tmp, "saved")
	if err := os.Rename(tmp, target); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got := collect(t, w)
	var targetCount int
	for _, ev := range got {
		if ev.Path == target {
			targetCount++
		}
	}
	if targetCount != 1 {
		t.Fatalf("want exactly 1 event for target %q, got %d: %+v", target, targetCount, got)
	}
	// On inotify the rename of the temp file is reported reliably, so
	// the ephemeral-file rule drops it and the burst collapses to a
	// single event. The kqueue backend (macOS) can miss or reorder
	// the temp's rename, so the temp may surface as a cheap
	// false-positive; the consumer re-derives truth regardless.
	if runtime.GOOS == "linux" && len(got) != 1 {
		t.Errorf("on inotify the temp file should coalesce away; got %+v", got)
	}
}

func TestIgnoredDirsProduceNoEvents(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(t, []watch.Root{{Project: "proj", Path: root}})

	for _, name := range []string{"node_modules", "vendor", ".guild"} {
		dir := filepath.Join(root, name)
		mkdir(t, dir)
		writeFile(t, filepath.Join(dir, "junk.txt"), "noise")
	}

	got := collect(t, w)
	for _, ev := range got {
		t.Errorf("unexpected event under ignored dir: %+v", ev)
	}
}

func TestRecursiveWatchOfRuntimeCreatedDir(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(t, []watch.Root{{Project: "proj", Path: root}})

	// A directory created after startup must be registered so files
	// later created inside it are observed.
	sub := filepath.Join(root, "pkg", "inner")
	mkdir(t, sub)
	time.Sleep(armDelay) // let the new-dir watch arm before writing into it

	target := filepath.Join(sub, "deep.go")
	writeFile(t, target, "package inner")

	got := collect(t, w)
	var found bool
	for _, ev := range got {
		if ev.Path == target && ev.Kind == watch.KindFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a file event for %q, got %+v", target, got)
	}
}

func TestCloseClosesEventChannelAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	w, err := watch.New([]watch.Root{{Project: "p", Path: root}}, watch.Options{
		Debounce: testDebounce,
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close should be a no-op error-wise: %v", err)
	}
	if _, ok := <-w.Events(); ok {
		t.Fatal("Events channel should be closed after Close")
	}
}

func TestMissingRootDegradesQuietly(t *testing.T) {
	good := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	w := newWatcher(t, []watch.Root{
		{Project: "good", Path: good},
		{Project: "missing", Path: missing},
	})

	// The missing root must not have killed the watcher: the good
	// root still emits.
	target := filepath.Join(good, "alive.txt")
	writeFile(t, target, "still here")

	got := collect(t, w)
	if len(got) != 1 || got[0].Path != target {
		t.Fatalf("want 1 event for good root, got %+v", got)
	}
}

func TestDefaultDebounceWhenUnset(t *testing.T) {
	root := t.TempDir()
	w, err := watch.New([]watch.Root{{Project: "p", Path: root}}, watch.Options{
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	// Nothing to assert beyond construction succeeding with the
	// zero-value debounce; the constant is exercised by the rest of
	// the suite through Options.Debounce.
	_ = watch.DefaultDebounce
}

// --- git HEAD watching ---

func gitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "nonexistent-gitconfig"),
		"GIT_AUTHOR_NAME=guild-test",
		"GIT_AUTHOR_EMAIL=guild-test@example.com",
		"GIT_COMMITTER_NAME=guild-test",
		"GIT_COMMITTER_EMAIL=guild-test@example.com",
	)
}

func runGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitHeadWatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	env := gitEnv(t)

	// Build a real repo with one commit BEFORE the watcher starts, so
	// only the test's own commit and branch switch are observed.
	runGit(t, repo, env, "init")
	writeFile(t, filepath.Join(repo, "README.md"), "# repo\n")
	runGit(t, repo, env, "add", "-A")
	runGit(t, repo, env, "commit", "-m", "initial")

	w := newWatcher(t, []watch.Root{{Project: "repo", Path: repo}})

	t.Run("commit emits one git_head", func(t *testing.T) {
		// An empty commit isolates the ref-update signal: it moves the
		// branch ref without touching any working-tree file, so the
		// only event a correct watcher emits is the git_head.
		runGit(t, repo, env, "commit", "--allow-empty", "-m", "second")

		assertExactlyOneGitHead(t, collect(t, w), repo)
	})

	t.Run("branch switch emits one git_head", func(t *testing.T) {
		runGit(t, repo, env, "checkout", "-b", "feature/work")

		assertExactlyOneGitHead(t, collect(t, w), repo)
	})
}

func assertExactlyOneGitHead(t *testing.T, got []watch.Event, repo string) {
	t.Helper()
	var heads int
	for _, ev := range got {
		if ev.Kind != watch.KindGitHead {
			t.Errorf("unexpected non-git event: %+v", ev)
			continue
		}
		heads++
		if ev.Path != repo {
			t.Errorf("git_head path = %q, want repo root %q", ev.Path, repo)
		}
		if ev.Project != "repo" {
			t.Errorf("git_head project = %q, want repo", ev.Project)
		}
	}
	if heads != 1 {
		t.Fatalf("want exactly 1 git_head event, got %d: %+v", heads, got)
	}
}
