package watch

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func budgetLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func awaitClosed(t *testing.T, w *Watcher) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-w.Events():
			if !ok {
				return
			}
		case <-deadline.C:
			t.Fatal("exhausted watcher did not close")
		}
	}
}

func TestInitialWatchBudgetReleasesBackend(t *testing.T) {
	root := t.TempDir()
	for i := range 12 {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprint(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w, err := New([]Root{{Project: "large", Path: root}}, Options{MaxWatchPaths: 4, Logger: budgetLogger()})
	if w != nil {
		_ = w.Close()
		t.Fatal("expected oversized registration to fail")
	}
	if !errors.Is(err, errWatchBudget) {
		t.Fatalf("expected budget failure: %v", err)
	}
	w, err = New([]Root{{Project: "small", Path: t.TempDir()}}, Options{MaxWatchPaths: 4, Logger: budgetLogger()})
	if err != nil {
		t.Fatalf("backend unusable after budget failure: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWatchBudgetBoundsGrowth(t *testing.T) {
	root := t.TempDir()
	w, err := New([]Root{{Project: "growing", Path: root}}, Options{MaxWatchPaths: 2, Logger: budgetLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	for _, name := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	awaitClosed(t, w)
}
