package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/daemon/watch"
)

// pipelineDebounce is short enough to keep the suite fast but comfortably
// longer than per-event delivery latency on inotify/kqueue, so a single
// logical change still coalesces into one normalized event.
const pipelineDebounce = 120 * time.Millisecond

// pipelineArmDelay gives fsnotify a moment to finish registering watches
// before the test mutates the tree.
const pipelineArmDelay = 80 * time.Millisecond

// runPipeline starts p.Run in a goroutine and returns a stop func that
// cancels it and waits for the loop to exit.
func runPipeline(t *testing.T, p *Pipeline) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// staticRoots returns a RootsFunc that always yields the given roots.
func staticRoots(roots ...watch.Root) RootsFunc {
	return func(context.Context) ([]watch.Root, error) { return roots, nil }
}

// waitFor polls cond until true or the deadline, failing the test on
// timeout. Keeps the event-driven assertions deterministic without a
// fixed sleep that would either flake or slow the suite.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPipelineDisabledNeverWatches proves a disabled pipeline starts no
// watcher, reports the disabled state, and exits cleanly on ctx cancel.
func TestPipelineDisabledNeverWatches(t *testing.T) {
	var processed atomic.Int64
	p := NewPipeline(PipelineConfig{
		Enabled: false,
		Roots:   staticRoots(),
		Process: func(context.Context, watch.Event) (EventResult, error) {
			processed.Add(1)
			return EventResult{}, nil
		},
		Logger: discardLogger(),
	})
	stop := runPipeline(t, p)
	defer stop()

	st := p.Status()
	if st.Enabled {
		t.Fatalf("disabled pipeline reports Enabled=true")
	}
	if st.Watching {
		t.Fatalf("disabled pipeline reports Watching=true")
	}
	if processed.Load() != 0 {
		t.Fatalf("disabled pipeline processed %d events, want 0", processed.Load())
	}
}

// TestPipelineMissingSeamsTreatedDisabled proves an Enabled pipeline with
// a nil Roots or Process seam degrades to disabled (inert) instead of
// crashing the daemon: a host wiring gap must never take serving down.
func TestPipelineMissingSeamsTreatedDisabled(t *testing.T) {
	p := NewPipeline(PipelineConfig{Enabled: true, Logger: discardLogger()})
	stop := runPipeline(t, p)
	defer stop()

	if got := p.Status(); got.Enabled {
		t.Fatalf("pipeline with nil seams reports Enabled=true; want treated as disabled")
	}
}

// TestPipelineFileEventFlagsAndCounts proves a real file change under a
// watched root produces exactly one debounced event, the ProcessFunc runs
// for it, and the status counters reflect the returned signals/renewals.
func TestPipelineFileEventFlagsAndCounts(t *testing.T) {
	root := t.TempDir()
	tracked := filepath.Join(root, "doc.md")
	if err := os.WriteFile(tracked, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	var (
		mu     sync.Mutex
		events []watch.Event
	)
	p := NewPipeline(PipelineConfig{
		Enabled:  true,
		Debounce: pipelineDebounce,
		Roots:    staticRoots(watch.Root{Project: "proj", Path: root}),
		Process: func(_ context.Context, ev watch.Event) (EventResult, error) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			// One signal, one renewal per file event, so the counters are
			// distinguishable from the event count.
			return EventResult{Signals: 1, QuestsPosted: 1}, nil
		},
		Logger: discardLogger(),
	})
	stop := runPipeline(t, p)
	defer stop()

	waitFor(t, "watcher to arm", func() bool { return p.Status().Watching })
	time.Sleep(pipelineArmDelay)

	if err := os.WriteFile(tracked, []byte("v2"), 0o600); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	waitFor(t, "the file event to be processed", func() bool {
		return p.Status().EventsSeen >= 1
	})

	st := p.Status()
	if st.ProjectsWatched != 1 {
		t.Fatalf("ProjectsWatched=%d, want 1", st.ProjectsWatched)
	}
	if st.SignalsRecorded < 1 {
		t.Fatalf("SignalsRecorded=%d, want >=1", st.SignalsRecorded)
	}
	if st.QuestsPosted < 1 {
		t.Fatalf("QuestsPosted=%d, want >=1", st.QuestsPosted)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatalf("ProcessFunc never ran")
	}
	for _, ev := range events {
		if ev.Project != "proj" {
			t.Fatalf("event project=%q, want proj", ev.Project)
		}
		if ev.Kind != watch.KindFile {
			t.Fatalf("event kind=%q, want file", ev.Kind)
		}
		if ev.Path != tracked {
			t.Fatalf("event path=%q, want %q", ev.Path, tracked)
		}
	}
}

// TestPipelineProcessErrorDoesNotCrash proves a ProcessFunc error is
// recorded and the event dropped, but the pipeline keeps watching and the
// daemon keeps serving: a single failed event only degrades to query-time
// staleness for that event.
func TestPipelineProcessErrorDoesNotCrash(t *testing.T) {
	root := t.TempDir()
	tracked := filepath.Join(root, "doc.md")
	if err := os.WriteFile(tracked, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	p := NewPipeline(PipelineConfig{
		Enabled:  true,
		Debounce: pipelineDebounce,
		Roots:    staticRoots(watch.Root{Project: "proj", Path: root}),
		Process: func(context.Context, watch.Event) (EventResult, error) {
			return EventResult{}, errors.New("flag stale exploded")
		},
		Logger: discardLogger(),
	})
	stop := runPipeline(t, p)
	defer stop()

	waitFor(t, "watcher to arm", func() bool { return p.Status().Watching })
	time.Sleep(pipelineArmDelay)

	if err := os.WriteFile(tracked, []byte("v2"), 0o600); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	waitFor(t, "the failing event to be recorded", func() bool {
		return p.Status().LastError != ""
	})

	st := p.Status()
	if !st.Watching {
		t.Fatalf("a ProcessFunc error must not stop watching; Watching=false")
	}
	if st.EventsSeen < 1 {
		t.Fatalf("EventsSeen=%d, want >=1 (the failing event still counts)", st.EventsSeen)
	}
	if st.SignalsRecorded != 0 || st.QuestsPosted != 0 {
		t.Fatalf("a failed event recorded signals/quests: signals=%d quests=%d", st.SignalsRecorded, st.QuestsPosted)
	}
}

// TestPipelineRootsErrorDegrades proves a roots-enumeration failure leaves
// the pipeline not-watching with a status breadcrumb, never crashing.
func TestPipelineRootsErrorDegrades(t *testing.T) {
	p := NewPipeline(PipelineConfig{
		Enabled: true,
		Roots: func(context.Context) ([]watch.Root, error) {
			return nil, errors.New("db locked")
		},
		Process: func(context.Context, watch.Event) (EventResult, error) {
			return EventResult{}, nil
		},
		Logger: discardLogger(),
	})
	stop := runPipeline(t, p)
	defer stop()

	waitFor(t, "the roots error to be recorded", func() bool {
		return p.Status().LastError != ""
	})
	st := p.Status()
	if st.Watching {
		t.Fatalf("roots error must degrade to not-watching; Watching=true")
	}
	if !st.Enabled {
		t.Fatalf("a transient roots error must not flip Enabled to false")
	}
}

// TestPipelineRescanPicksUpNewProject proves a project registered after
// the daemon started begins being watched on the next rescan tick, driven
// by the fake clock so the cadence is deterministic.
func TestPipelineRescanPicksUpNewProject(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()

	var mu sync.Mutex
	roots := []watch.Root{{Project: "a", Path: rootA}}
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())

	p := NewPipeline(PipelineConfig{
		Enabled:        true,
		Debounce:       pipelineDebounce,
		RescanInterval: time.Minute,
		Roots: func(context.Context) ([]watch.Root, error) {
			mu.Lock()
			defer mu.Unlock()
			out := make([]watch.Root, len(roots))
			copy(out, roots)
			return out, nil
		},
		Process: func(context.Context, watch.Event) (EventResult, error) {
			return EventResult{}, nil
		},
		Logger: discardLogger(),
		clock:  clk,
	})
	stop := runPipeline(t, p)
	defer stop()

	waitFor(t, "first generation to watch one project", func() bool {
		return p.Status().ProjectsWatched == 1
	})

	// Register a second project, then deliver a rescan tick.
	mu.Lock()
	roots = append(roots, watch.Root{Project: "b", Path: rootB})
	mu.Unlock()
	clk.tick()

	waitFor(t, "rescan to pick up the new project", func() bool {
		return p.Status().ProjectsWatched == 2
	})
}

// TestPipelineEmptyRootsHealthy proves zero registered projects is a
// healthy state (watching the empty set), not an error: the next rescan
// picks up the first registration.
func TestPipelineEmptyRootsHealthy(t *testing.T) {
	p := NewPipeline(PipelineConfig{
		Enabled: true,
		Roots:   staticRoots(),
		Process: func(context.Context, watch.Event) (EventResult, error) {
			return EventResult{}, nil
		},
		Logger: discardLogger(),
	})
	stop := runPipeline(t, p)
	defer stop()

	// Give Run a moment to build the first (empty) generation.
	waitFor(t, "the pipeline to settle on empty roots", func() bool {
		st := p.Status()
		return st.Enabled && st.ProjectsWatched == 0 && st.LastError == ""
	})
}
