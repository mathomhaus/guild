package mcp

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/sleep"
)

// sleep_autopass.go is the in-process MCP server's degraded-mode trigger
// for the sleep cycle. ADR-005 Part 2 keeps daemon.autostart on from day
// one, but the no-daemon path is supported forever: a machine where
// autostart is disabled, or where the daemon spawn failed, should still
// get a slow drip of consolidation instead of none. This file fires ONE
// bounded dream pass per process from the first guild_session_start when
// no daemon is resident.
//
// It is a deliberate copy of the embed_autobackfill.go contract:
//
//   - Fires EXACTLY ONCE per MCP server process (sync.Once).
//   - Never blocks the caller: maybeTriggerSleepAutopass returns
//     immediately and all work happens in a background goroutine on
//     context.Background() because the pass is server-lifetime work, not
//     scoped to the session_start call that triggered it.
//   - Cross-process races are tolerated: the sleep runner journals every
//     mutation and steps gate through the HYBRID policy, so a daemon pass
//     and an autopass touching the same DB converge on additive writes.
//
// Gating, in the order the design spec lists:
//
//	(a) sleep enabled in config and GUILD_NO_SLEEP unset (config.Load
//	    folds both into Sleep.Enabled);
//	(b) no live daemon: when the daemon is up it owns the idle scheduler,
//	    so an autopass would double-dream. daemon.Probe is the same
//	    liveness check the shim uses, and it correctly suppresses the
//	    autopass in BOTH no-daemon-but-served-in-daemon cases: a session
//	    served by the daemon's own per-connection server probes its parent
//	    daemon as alive and skips;
//	(c) throttle: handled inside sleep.Autopass against the journal's last
//	    pass (sleepAutopassMinInterval), so a briefly-stopped daemon does
//	    not cause the next session-start to re-dream;
//	(d) journal table present: a pre-migration DB surfaces as a journal
//	    read error inside sleep.Autopass, which degrades to running the
//	    pass; BeginPass then no-ops cleanly if the table is truly absent.

// sleepAutopassBudget is the wall budget for one degraded-mode pass. It
// is much tighter than the daemon's idle-pass budget
// (config.SleepConfig.PassBudgetSeconds, default 60s) because an
// autopass steals cycles from an active session's machine rather than
// running on a deliberately-idle daemon. The runner cancels any step
// that overruns and journals the pass as partial, so the session never
// pays more than this.
const sleepAutopassBudget = 10 * time.Second

// Autopass mutation caps, tighter than the daemon idle caps
// (sleepPassMaxAutoOps=50 / sleepPassMaxRenewalPosts=5 in daemon.go). An
// autopass drips one auto-meld and one renewal post per fire; a backlog
// drains over successive sessions instead of flooding the board or the
// journal in the middle of an active session.
const (
	sleepAutopassMaxAutoOps      = 1
	sleepAutopassMaxQuestPosts   = 1
	sleepAutopassMaxRenewalPosts = 1
)

// sleepAutopassMinInterval throttles the degraded pass: if the journal's
// most recent pass (from either trigger) ended within this window, the
// autopass skips. Six hours matches the spec default. It keeps a daemon
// that was briefly stopped from causing the next in-process session to
// repeat work the daemon already did, and keeps a burst of fresh
// sessions on a daemonless machine from each firing a pass.
const sleepAutopassMinInterval = 6 * time.Hour

// sleepAutopassProbeTimeout bounds the daemon liveness dial in the gate.
// Mirrors cmd/guild/mcp.go's shimProbeTimeout: with no daemon the probe
// is a single failed read of ~/.guild/daemon.json and never dials, so
// this only caps the one unix dial made when a discovery record exists.
const sleepAutopassProbeTimeout = 250 * time.Millisecond

// sleepAutopassGate owns the once-per-process guard for the degraded
// pass. sync.Once semantics: however many session_starts race through
// the trigger at process startup, the body runs exactly once.
type sleepAutopassGate struct {
	once sync.Once

	// doneCh is closed when the spawned pass goroutine has completed
	// (ran, throttled, or errored). Nil until the trigger fires; written
	// inside once.Do so post-trigger readers (tests) do not race it.
	doneCh chan struct{}
}

// processSleepAutopassGate is the package-default gate behind
// maybeTriggerSleepAutopass. One per process; reset between test-spawned
// servers via resetSleepAutopassState, exactly as the auto-backfill gate
// is reset.
var processSleepAutopassGate = &sleepAutopassGate{}

// resetSleepAutopassState swaps in a fresh gate so the next process (or
// the next test-spawned server) can fire the trigger again. Called from
// Register so each default server construction starts with a clean gate,
// mirroring how the auto-backfill state is reset; tests also call it
// directly.
func resetSleepAutopassState() {
	processSleepAutopassGate = &sleepAutopassGate{}
}

// sleepAutopassConfigLoad resolves the sleep config gate. Indirected so
// tests can force enabled/disabled without writing a config file. The
// production implementation reads the merged config, which folds both
// [sleep] enabled and GUILD_NO_SLEEP into Sleep.Enabled.
var sleepAutopassConfigLoad = func() config.SleepConfig {
	cfg, err := config.Load(nil)
	if err != nil {
		// A broken config file must not fire an autopass: the safe
		// degraded behavior is "do not dream", matching the opt-out path.
		return config.SleepConfig{Enabled: false}
	}
	return cfg.Sleep
}

// sleepAutopassDaemonRunning reports whether a live guild daemon is
// resident. Indirected so tests can simulate daemon-up / daemon-down
// without a real socket. Production probes the discovery file the same
// way the shim does; any probe error is treated as "running" so a
// flaky probe errs toward NOT double-dreaming.
var sleepAutopassDaemonRunning = func() bool {
	res, _, err := daemon.Probe(binaryVersion, sleepAutopassProbeTimeout)
	if err != nil {
		return true
	}
	return res != daemon.NotRunning
}

// sleepAutopassGoroutineHook is a test-only barrier invoked at the head
// of the background pass goroutine. Production leaves it nil; a test
// assigns a blocking func to prove maybeTriggerSleepAutopass returns
// before the pass runs, then releases it.
var sleepAutopassGoroutineHook func()

// maybeTriggerSleepAutopass fires the package-default gate from
// guild_session_start. It returns immediately; the pass (if any) runs in
// a background goroutine. embed is the server's shared lore-side embed
// provider (the same one the auto-backfill trigger uses); nil is fine
// and resolves to BM25-only. logger is the server-scoped logger; nil
// defaults to slog.Default().
func maybeTriggerSleepAutopass(embed *embedProvider, logger *slog.Logger) {
	processSleepAutopassGate.maybeTrigger(embed, logger)
}

// maybeTrigger applies the cheap, synchronous gates (config + daemon
// liveness) and, if both pass, spawns the once-guarded background pass.
// The gates run BEFORE once.Do so a disabled-or-daemon-up process does
// not burn its single Once on a no-op: a daemon that later stops leaves
// the gate unfired, and the next session can still fire once the daemon
// is gone.
func (g *sleepAutopassGate) maybeTrigger(embed *embedProvider, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	// (a) sleep disabled (config [sleep] enabled=false or GUILD_NO_SLEEP):
	// the in-process path must be byte-identical to a build without sleep.
	if !sleepAutopassConfigLoad().Enabled {
		return
	}

	// (b) a daemon owns the idle scheduler: do not also autopass.
	if sleepAutopassDaemonRunning() {
		return
	}

	// Resolve the DB paths synchronously, while the caller's environment
	// is still in scope: the background goroutine must not re-resolve
	// $HOME later (a long-lived pass that outlived a path-rebinding caller
	// could otherwise open the wrong store). A resolve failure aborts the
	// trigger without burning the Once.
	lorePath, err := ldbPath()
	if err != nil {
		logger.Warn("sleep autopass: resolve lore db path failed", slog.String("err", err.Error()))
		return
	}
	questPath, err := qdbPath()
	if err != nil {
		logger.Warn("sleep autopass: resolve quest db path failed", slog.String("err", err.Error()))
		return
	}

	g.once.Do(func() {
		done := make(chan struct{})
		g.doneCh = done
		// context.Background(): the pass is server-lifetime work that
		// outlives the session_start call which triggered it. The wall
		// budget inside sleep.Autopass bounds it; nothing here blocks the
		// handler.
		go runSleepAutopass(context.Background(), lorePath, questPath, embed, logger, done)
	})
}

// runSleepAutopass is the body of the once-guarded trigger: open fresh
// db handles (the per-call open discipline every MCP tool uses; WAL
// makes it cheap), resolve the shared embedder if present, and run the
// degraded pass under the autopass budget and caps. With the step
// registry empty this journals a pass row with zero steps and returns.
//
// Every failure degrades to a log line: an autopass must never surface
// an error to the session it was triggered from, and the handler has
// already returned by the time this runs.
func runSleepAutopass(ctx context.Context, lorePath, questPath string, embed *embedProvider, logger *slog.Logger, done chan struct{}) {
	defer close(done)

	// Test-only barrier: lets a test hold the background pass open while
	// it asserts the trigger already returned. nil in production.
	if sleepAutopassGoroutineHook != nil {
		sleepAutopassGoroutineHook()
	}

	// Open against the paths resolved synchronously at trigger time, so
	// the pass cannot drift onto a different store if the caller's $HOME
	// changed after the trigger returned.
	loreDB, err := openDB(ctx, lorePath, "lore")
	if err != nil {
		logger.Warn("sleep autopass: open lore db failed", slog.String("err", err.Error()))
		return
	}
	defer func() { _ = loreDB.Close() }()

	questDB, err := openDB(ctx, questPath, "quest")
	if err != nil {
		logger.Warn("sleep autopass: open quest db failed", slog.String("err", err.Error()))
		return
	}
	defer func() { _ = questDB.Close() }()

	// Resolve the lore-side embed deps from the server's shared provider,
	// the same observation point the auto-backfill trigger uses. A nil
	// provider or a nil resolve is the documented BM25-only fallback the
	// embed step tolerates as a clean no-op.
	var embedDeps *lore.EmbedDeps
	if embed != nil {
		embedDeps = embed.ResolveEmbedDeps(ctx)
	}

	res, ran, err := sleep.Autopass(ctx, sleep.AutopassConfig{
		LoreDB:      loreDB,
		QuestDB:     questDB,
		Embed:       embedDeps,
		Budget:      sleepAutopassBudget,
		MinInterval: sleepAutopassMinInterval,
		Logger:      logger,
		Caps: sleep.Caps{
			MaxAutoOps:      sleepAutopassMaxAutoOps,
			MaxQuestPosts:   sleepAutopassMaxQuestPosts,
			MaxRenewalPosts: sleepAutopassMaxRenewalPosts,
		},
	})
	if err != nil {
		logger.Warn("sleep autopass: pass failed", slog.String("err", err.Error()))
		return
	}
	if !ran {
		// Throttled: a recent pass already did the work.
		return
	}
	if res != nil {
		logger.Info("sleep autopass complete",
			slog.Int64("pass_id", res.PassID),
			slog.Bool("partial", res.Partial),
			slog.Int("steps", len(res.Steps)),
		)
	}
}
