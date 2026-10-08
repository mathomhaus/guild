package daemon

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mathomhaus/guild/internal/daemon/watch"
)

// This file is the daemon's watch -> staleness -> renewal pipeline
// (ADR-005 Phase 4). It is the change that makes self-renewing knowledge
// user-visible: the resident daemon watches every registered project
// root, and when a tracked source file changes (or a repo's HEAD moves)
// the lore entries citing that path get flagged stale and a capped,
// deduplicated renewal bounty appears on the board within seconds,
// instead of waiting for the next idle dream pass or a query-time check.
//
// The pipeline only decides WHEN to act (a debounced file or git event)
// and threads the counters for status. WHAT one event does (flag stale,
// post renewals) lives behind the ProcessFunc seam the host supplies, so
// internal/daemon stays a leaf and never imports internal/lore,
// internal/quest, internal/project, or internal/mcp. This mirrors the
// idle scheduler's PassFunc split exactly.
//
// Hard invariants from ADR-005 carried here:
//
//   - Zero impact on the no-daemon path: nothing in this file runs unless
//     `guild daemon run` started a Pipeline with watch enabled. Correctness
//     never depends on it; with the watcher off (or crashed) staleness
//     falls back to the query-time echo check, byte-identical to today.
//   - Additive only: a file event writes staleness signals and posts
//     renewal quests, both additive. The destructive judgment
//     (re-validate vs. supersede vs. retire) is what the renewal quest
//     itself routes to a human or interactive agent. So the pipeline runs
//     unattended with every action journaled by the host's ProcessFunc.
//   - A watcher crash or backend error never takes down the daemon: the
//     watch package degrades quietly (dropped events, skipped roots), and
//     a New failure here is logged and degrades the pipeline to inert
//     while the daemon keeps serving.

// defaultRescanInterval is how often a watch-enabled pipeline re-enumerates
// project roots and rebuilds its watcher, so a project registered after the
// daemon started begins producing events without a daemon restart. A minute
// is cheap (one project.List query plus a tree walk) relative to the renewal
// cadence and well under any user's tolerance for "I added a project, why is
// nothing watching it".
const defaultRescanInterval = time.Minute

// minRescanInterval floors the rescan cadence so a tiny configured interval
// (or a fast-clock test) cannot spin the loop rebuilding watchers.
const minRescanInterval = time.Second

// EventResult is what one processed watch event recorded: how many lore
// staleness signals were written and how many renewal quests were posted.
// The host's ProcessFunc returns it so the pipeline can keep the status
// counters without knowing anything about lore or quest internals.
type EventResult struct {
	// Signals is the number of staleness signal rows written or refreshed
	// for this event (the count of current entries citing the changed
	// path, or git-flagged in the sweep).
	Signals int
	// QuestsPosted is the number of renewal quests minted for this event
	// (after the cap and dedupe the host applies).
	QuestsPosted int
}

// ProcessFunc handles one debounced watch event end to end: for a file
// event it flags the citing lore entries stale and posts capped,
// deduplicated renewal quests; for a git_head event it runs the
// project-scoped git sweep and posts renewals for the hits. The host
// supplies it (internal/mcp's DaemonHost): it owns the db handles, the
// renewal cap, and journaling every signal and post, so internal/daemon
// never imports the mutation logic.
//
// It must honor ctx cancellation promptly (daemon shutdown). An error is
// logged and the event is dropped; the next event (or the query-time
// check) re-derives truth, so a single failed event only degrades to the
// behavior the no-watcher path already has.
type ProcessFunc func(ctx context.Context, ev watch.Event) (EventResult, error)

// RootsFunc enumerates the project roots to watch as of now. The host
// implements it over project.List against the lore db; the pipeline calls
// it at start and on every rescan so projects registered after the daemon
// started are picked up without a restart. An error is logged and the
// previous root set is kept (a transient db hiccup must not blank the
// watcher).
type RootsFunc func(ctx context.Context) ([]watch.Root, error)

// PipelineConfig parameterizes a Pipeline. The daemon resolves these from
// config.DaemonConfig; the zero value never watches (Enabled false).
type PipelineConfig struct {
	// Enabled gates the whole pipeline. False means Run returns
	// immediately after recording the disabled state, so status still
	// answers and no watcher goroutine ever starts.
	Enabled bool
	// Roots enumerates the project roots to watch. Required when Enabled.
	Roots RootsFunc
	// Process handles one debounced event (flag stale + post renewals).
	// Required when Enabled.
	Process ProcessFunc
	// Debounce is the watcher's quiet window per normalized event.
	// Non-positive uses the watch package default (one second).
	Debounce time.Duration
	// RescanInterval is how often roots are re-enumerated to pick up
	// newly registered projects. Non-positive uses defaultRescanInterval;
	// it is floored at minRescanInterval.
	RescanInterval time.Duration
	// Logger receives start/stop and degradation diagnostics. Nil falls
	// back to slog.Default(); never logs to stdout.
	Logger *slog.Logger
	// clock is injected by tests. nil uses the real clock (the same
	// scheduler clock seam).
	clock clock
}

// PipelineStatus is a snapshot of the watcher pipeline for daemon status.
// All counts are cumulative since the daemon started.
type PipelineStatus struct {
	// Enabled mirrors PipelineConfig.Enabled: false means no watcher was
	// ever started (config or env opt-out).
	Enabled bool
	// Watching is true while a watcher is live. It drops to false if the
	// watcher could not be (re)built, the visible "degraded to query-time
	// staleness" signal.
	Watching bool
	// ProjectsWatched is the number of project roots the current watcher
	// generation covers.
	ProjectsWatched int
	// EventsSeen counts debounced events consumed since start.
	EventsSeen int64
	// SignalsRecorded counts staleness signal rows written across all
	// events since start.
	SignalsRecorded int64
	// QuestsPosted counts renewal quests minted across all events since
	// start.
	QuestsPosted int64
	// LastError is the most recent watcher (re)build or event-processing
	// error, empty when none. It does not clear on success: it is the
	// breadcrumb for "the watcher hiccuped at some point".
	LastError string
}

// Pipeline owns the daemon's project watcher and the goroutine that turns
// its debounced events into staleness signals and renewal quests. It is
// safe for concurrent use: Status reads counters that Run's goroutine
// writes. Construct with NewPipeline, drive with Run.
type Pipeline struct {
	cfg PipelineConfig
	log *slog.Logger
	clk clock

	// Cumulative counters, written only by Run's consume goroutine and
	// read by Status; atomics so Status needs no lock.
	eventsSeen      atomic.Int64
	signalsRecorded atomic.Int64
	questsPosted    atomic.Int64

	mu              sync.Mutex
	watching        bool
	projectsWatched int
	lastErr         string
}

// NewPipeline validates cfg and returns a runnable Pipeline. A nil Roots
// or Process with Enabled true is treated as disabled (Run records it and
// returns) rather than panicking, so a host wiring gap degrades the
// daemon to no-watcher instead of crashing it.
func NewPipeline(cfg PipelineConfig) *Pipeline {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	clk := cfg.clock
	if clk == nil {
		clk = realClock{}
	}
	return &Pipeline{cfg: cfg, log: log, clk: clk}
}

// Status returns a snapshot of the watcher pipeline for daemon status.
func (p *Pipeline) Status() PipelineStatus {
	p.mu.Lock()
	watching := p.watching
	projects := p.projectsWatched
	lastErr := p.lastErr
	p.mu.Unlock()
	return PipelineStatus{
		Enabled:         p.enabled(),
		Watching:        watching,
		ProjectsWatched: projects,
		EventsSeen:      p.eventsSeen.Load(),
		SignalsRecorded: p.signalsRecorded.Load(),
		QuestsPosted:    p.questsPosted.Load(),
		LastError:       lastErr,
	}
}

// enabled reports whether the pipeline is fully wired to watch. A missing
// Roots or Process seam counts as disabled so a wiring gap is inert, not
// a crash.
func (p *Pipeline) enabled() bool {
	return p.cfg.Enabled && p.cfg.Roots != nil && p.cfg.Process != nil
}

// Run drives the watcher and its consume loop until ctx is cancelled
// (daemon shutdown). A disabled pipeline records the disabled state and
// returns immediately after ctx is cancelled, so the daemon's WaitGroup
// bookkeeping still completes without spinning. Run blocks; the daemon
// starts it in its own goroutine alongside the scheduler.
//
// The loop owns one watcher generation at a time. On start (and on every
// rescan tick) it enumerates roots and rebuilds the watcher; a build
// failure degrades the pipeline to not-watching with a status line and
// the daemon keeps serving. Events are consumed and handed to the
// ProcessFunc; counters update as they land.
func (p *Pipeline) Run(ctx context.Context) {
	if !p.enabled() {
		p.log.Info("daemon: watch pipeline disabled", "enabled", p.cfg.Enabled)
		<-ctx.Done()
		return
	}

	interval := p.cfg.RescanInterval
	if interval <= 0 {
		interval = defaultRescanInterval
	}
	if interval < minRescanInterval {
		interval = minRescanInterval
	}

	p.log.Info("daemon: watch pipeline started",
		"debounce", p.effectiveDebounce().String(),
		"rescan", interval.String(),
	)

	tk := p.clk.NewTicker(interval)
	defer tk.Stop()

	// First generation: build now so the daemon is watching before the
	// first rescan tick. A nil watcher means the build failed (logged);
	// the next tick retries.
	w := p.rebuild(ctx, nil)
	defer func() {
		if w != nil {
			_ = w.Close()
		}
	}()

	for {
		var events <-chan watch.Event
		if w != nil {
			events = w.Events()
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			w = p.rebuild(ctx, w)
		case ev, ok := <-events:
			if !ok {
				// The watcher's event channel closed without us closing it:
				// the watch loop exited (backend gone). Degrade to
				// not-watching and let the next rescan tick rebuild.
				p.markWatcherLost(w)
				w = nil
				continue
			}
			p.handle(ctx, ev)
		}
	}
}

// effectiveDebounce resolves the configured debounce against the watch
// package default for logging and watcher construction.
func (p *Pipeline) effectiveDebounce() time.Duration {
	if p.cfg.Debounce > 0 {
		return p.cfg.Debounce
	}
	return watch.DefaultDebounce
}

// rebuild closes the previous watcher (if any), enumerates the current
// roots, and starts a fresh watcher over them. It returns the new watcher,
// or nil when roots could not be enumerated or the watcher could not be
// built; either case leaves the pipeline not-watching until the next
// rescan, with a status line, and never returns an error to the caller
// because a watcher failure must not take the daemon down.
func (p *Pipeline) rebuild(ctx context.Context, prev *watch.Watcher) *watch.Watcher {
	if prev != nil {
		_ = prev.Close()
	}

	roots, err := p.cfg.Roots(ctx)
	if err != nil {
		// A transient enumeration failure: keep serving, retry next tick.
		p.setDegraded("enumerate project roots: " + err.Error())
		p.log.Warn("daemon: watch pipeline could not enumerate roots; degraded to query-time staleness",
			"err", err.Error())
		return nil
	}
	if len(roots) == 0 {
		// No registered projects yet: nothing to watch, but the pipeline
		// is healthy. The next rescan picks up the first registration.
		p.setWatching(0)
		return nil
	}

	w, err := watch.New(roots, watch.Options{
		Debounce: p.cfg.Debounce,
		Logger:   p.log,
	})
	if err != nil {
		p.setDegraded("start watcher: " + err.Error())
		p.log.Warn("daemon: watch pipeline could not start watcher; degraded to query-time staleness",
			"err", err.Error())
		return nil
	}

	p.setWatching(len(roots))
	p.log.Info("daemon: watch pipeline watching projects", "projects", len(roots))
	return w
}

// handle processes one debounced event: hand it to the ProcessFunc and
// fold the result into the counters. A ProcessFunc error is logged and the
// event dropped; the next event (or query-time check) re-derives truth.
func (p *Pipeline) handle(ctx context.Context, ev watch.Event) {
	p.eventsSeen.Add(1)
	res, err := p.cfg.Process(ctx, ev)
	if err != nil {
		// Cancellation is daemon shutdown, not a failure; the loop's
		// ctx.Done branch handles the exit.
		if ctx.Err() != nil {
			return
		}
		p.setLastErr("process event: " + err.Error())
		p.log.Warn("daemon: watch pipeline event failed; dropping",
			"project", ev.Project, "path", ev.Path, "kind", string(ev.Kind), "err", err.Error())
		// Record the failed staleness-renewal so the event stream shows
		// dropped events (ADR-006 Phase 5). No-op unless a recorder is wired.
		recordDecision(Decision{
			Kind:   DecisionStaleRenew,
			Allow:  false,
			Reason: "process_error",
			At:     p.clk.Now(),
			Inputs: map[string]bool{"errored": true, "signals_written": false, "quests_posted": false},
		})
		return
	}
	if res.Signals > 0 {
		p.signalsRecorded.Add(int64(res.Signals))
	}
	if res.QuestsPosted > 0 {
		p.questsPosted.Add(int64(res.QuestsPosted))
	}
	// Record the staleness-renewal decision (ADR-006 Phase 5). The
	// ProcessFunc already flagged staleness and posted renewals; res is its
	// result and we only snapshot it. Allow is "did this event lead to
	// renewal action" (signals or quests). No-op unless a recorder is wired,
	// so the disabled path is byte-identical.
	recordDecision(Decision{
		Kind:   DecisionStaleRenew,
		Allow:  res.Signals > 0 || res.QuestsPosted > 0,
		Reason: staleRenewReason(res),
		At:     p.clk.Now(),
		Inputs: map[string]bool{
			"errored":         false,
			"signals_written": res.Signals > 0,
			"quests_posted":   res.QuestsPosted > 0,
		},
		Metrics: map[string]int{
			"signals": res.Signals,
			"quests":  res.QuestsPosted,
		},
	})
	p.log.Info("daemon: watch event processed",
		"project", ev.Project,
		"path", ev.Path,
		"kind", string(ev.Kind),
		"signals", res.Signals,
		"renewals", res.QuestsPosted,
	)
}

// markWatcherLost records that the event channel closed unexpectedly and
// closes the dead watcher's resources. The next rescan tick rebuilds.
func (p *Pipeline) markWatcherLost(w *watch.Watcher) {
	if w != nil {
		_ = w.Close()
	}
	p.setDegraded("watcher event stream closed; will retry on next rescan")
	p.log.Warn("daemon: watch pipeline event stream closed; degraded to query-time staleness until rescan")
}

// setWatching marks the pipeline live over n roots and clears the
// not-watching state. It does not clear lastErr: a prior hiccup stays
// visible as a breadcrumb.
func (p *Pipeline) setWatching(n int) {
	p.mu.Lock()
	p.watching = true
	p.projectsWatched = n
	p.mu.Unlock()
}

// setDegraded marks the pipeline not-watching and records why.
func (p *Pipeline) setDegraded(reason string) {
	p.mu.Lock()
	p.watching = false
	p.projectsWatched = 0
	p.lastErr = reason
	p.mu.Unlock()
}

// setLastErr records the most recent error without changing the watching
// state (the watcher is still live; one event just failed).
func (p *Pipeline) setLastErr(reason string) {
	p.mu.Lock()
	p.lastErr = reason
	p.mu.Unlock()
}

// staleRenewReason summarizes a processed event's outcome for the decision
// record's reason. It only describes what the ProcessFunc already did.
func staleRenewReason(res EventResult) string {
	switch {
	case res.QuestsPosted > 0 && res.Signals > 0:
		return "flagged_stale_and_posted_renewals"
	case res.QuestsPosted > 0:
		return "posted_renewals"
	case res.Signals > 0:
		return "flagged_stale"
	default:
		return "no_citing_entries"
	}
}
