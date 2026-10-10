package watch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Lower the descriptor limit only in a subprocess. A real kqueue Add can
// partially open its directory's files before returning EMFILE; all those
// watches must close before core daemon services continue.
func TestKqueueExhaustionReleasesPartialRegistration(t *testing.T) {
	if os.Getenv("GUILD_WATCH_FD_FIXTURE") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKqueueExhaustionReleasesPartialRegistration$") //nolint:gosec // re-exec this test with a fixed selector
		cmd.Env = append(os.Environ(), "GUILD_WATCH_FD_FIXTURE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("low-descriptor fixture: %v\n%s", err, out)
		}
		return
	}
	warm, err := New([]Root{{Project: "warm", Path: t.TempDir()}}, Options{Logger: budgetLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := warm.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = 64
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	var held []*os.File
	for {
		f, openErr := os.Open(filepath.Join(root, "a"))
		if openErr != nil {
			if !errors.Is(openErr, syscall.EMFILE) {
				t.Fatal(openErr)
			}
			break
		}
		held = append(held, f)
	}
	if len(held) < 6 {
		t.Fatal("not enough descriptors for fixture")
	}
	for _, f := range held[len(held)-6:] {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	held = held[:len(held)-6]
	w, watchErr := New([]Root{{Project: "limited", Path: root}}, Options{Logger: budgetLogger()})
	if w != nil {
		_ = w.Close()
	}
	for _, f := range held {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(watchErr, syscall.EMFILE) {
		t.Fatalf("expected native EMFILE, got %v", watchErr)
	}
	deadline := time.Now().Add(time.Second)
	for {
		after, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Fatal(err)
		}
		if len(after) <= len(baseline) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial registration leaked descriptors: before %d, after %d", len(baseline), len(after))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
