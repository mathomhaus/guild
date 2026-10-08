package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/cli"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/mcp"
	"github.com/mathomhaus/guild/internal/module"
)

// wireDaemonRun wires the `guild daemon run` cobra RunE onto
// internal/cli's daemonRunCmd placeholder, mirroring wireMCPServe in
// mcp.go: root.go stays daemon-agnostic, and the assembly of the
// daemon listener + MCP session host happens only in the cmd/guild
// build, where both packages are already linked.
func wireDaemonRun() {
	cli.SetDaemonRunRunE(runDaemonRun)
}

// init runs at binary startup, before cobra.Execute. Same rationale as
// mcp.go's init: tests that import internal/cli directly keep the
// errNotImplemented placeholders and never pull in the SDK. The
// lifecycle verbs (start/stop/status/restart) ride the same seam: the
// command definitions live in internal/cli/daemon_cmd.go, the handlers
// here.
func init() {
	wireDaemonRun()
	cli.SetDaemonLifecycleRunEs(runDaemonStart, runDaemonStop, runDaemonStatus, runDaemonRestart)
}

// runDaemonRun is the cobra RunE for `guild daemon run`. Signal
// handling mirrors runMCPServe: a [signal.NotifyContext] on SIGINT +
// SIGTERM cancels the daemon's ctx, which closes the listener, ends
// every in-flight session, and removes the socket + discovery file
// before Run returns nil, so Ctrl-C and `kill <pid>` exit 0 with a
// clean ~/.guild.
func runDaemonRun(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Resolve config once for every background loop: the [sleep] idle
	// dream-pass scheduler, the [daemon] watch -> staleness -> renewal
	// pipeline, and the [daemon] session registry + lease heartbeat. A
	// config-load failure should not block the daemon from serving, so each
	// builder degrades to a disabled loop with a warning and the host falls
	// back to built-in lease timings.
	cfg := loadDaemonConfig()

	// One host per daemon process: the shared provider bundle behind every
	// session, plus the session registry whose heartbeat tick refreshes
	// live sessions' leases. The lease TTL and heartbeat interval come from
	// config (built-in defaults when config failed to load). The daemon
	// server itself stays MCP-agnostic and receives the session handler and
	// registry as seams.
	host := buildDaemonHost(cfg)

	// Idle dream-pass scheduler (ADR-005 Phase 2): the host supplies the
	// per-pass runner (WHAT a pass does); the scheduler decides WHEN.
	scheduler := buildSleepScheduler(host, cfg)

	// Watch -> staleness -> renewal pipeline (ADR-005 Phase 4): the host
	// supplies the roots enumerator and the per-event processor (WHAT one
	// event does); the pipeline decides WHEN (a debounced file/git event).
	pipeline := buildWatchPipeline(host, cfg)

	srv, err := daemon.NewServer(daemon.Config{
		Version:  version,
		Sessions: host.ServeSession,
		// Terminal CLI verbs routed over the JSON-exec RPC execute here
		// with CLI-equivalent Deps; the shared embed providers keep one
		// embedder per daemon process across MCP sessions and routed
		// CLI searches alike.
		Exec:          cli.NewDaemonExecHandler(host.QuestEmbedSource(), host.LoreEmbedSource()),
		EmbedderState: host.EmbedderState,
		Scheduler:     scheduler,
		Pipeline:      pipeline,
		// Session registry + lease heartbeat (ADR-005 Phase 3): the host
		// owns the registry (ServeSession registers each connection on it);
		// the daemon Server drives its tick for the daemon's lifetime.
		Registry: host.Registry(),
		// Lease reaper (ADR-005 Phase 3): the host owns the reaper (its
		// sweep consults the registry for session liveness and forfeits the
		// zombie claim behind any lapsed lease); the daemon Server drives
		// its sweep tick for the daemon's lifetime.
		Reaper: host.Reaper(),
		// Module-contributed background loops (ADR-006 Phase 3): every
		// enabled capability module's Services() join the daemon's uniform
		// service registry alongside the built-in loops above. Empty today
		// (no shipped module ships a loop), so daemon behavior is
		// unchanged; a disabled module contributes nothing because
		// module.Enabled drops it before Services() is consulted.
		Services: enabledModuleServices(cfg),
		Logger:   host.Logger(),
	})
	if err != nil {
		return fmt.Errorf("daemon run: %w", err)
	}

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("daemon run: %w", err)
	}
	return nil
}

// enabledModuleServices returns the daemon background loops contributed
// by every enabled capability module (ADR-006 Phase 3). It walks the
// module registry under the same config-backed predicate the MCP and CLI
// surfaces use, so a module disabled via [modules] / GUILD_MODULE_* never
// has its loops started. A nil cfg (config-load failure) means every
// module on its own default, matching the degrade posture elsewhere. No
// shipped core module returns a Service today, so this is an empty slice
// on the default path and daemon startup is byte-identical.
func enabledModuleServices(cfg *config.Config) []module.Service {
	var svcs []module.Service
	for _, m := range module.Enabled(config.ModuleEnabled(cfg)) {
		svcs = append(svcs, m.Services()...)
	}
	return svcs
}

// loadDaemonConfig loads the merged config for the daemon's background
// loops. Config load happens with nil flags (the daemon run command has
// no sleep/watch flags); a load failure returns nil and the loop builders
// fall back to disabled (and the host to built-in lease timings), so a bad
// config never blocks the daemon from serving. It runs before the host is
// built, so a failure is logged to the package logger rather than the
// host's.
func loadDaemonConfig() *config.Config {
	cfg, err := config.Load(nil)
	if err != nil {
		slog.Warn("daemon: config load failed; idle dream passes, watcher, and lease heartbeats use built-in defaults", "err", err.Error())
		return nil
	}
	return cfg
}

// buildDaemonHost constructs the per-process MCP host with lease timings
// resolved from config. A nil cfg (load failure) falls back to the host's
// built-in default lease TTL and heartbeat interval.
func buildDaemonHost(cfg *config.Config) *mcp.DaemonHost {
	if cfg == nil {
		return mcp.NewDaemonHost()
	}
	return mcp.NewDaemonHostWithLeases(
		time.Duration(cfg.Daemon.LeaseTTLSeconds)*time.Second,
		time.Duration(cfg.Daemon.HeartbeatIntervalSeconds)*time.Second,
		time.Duration(cfg.Daemon.ReapIntervalSeconds)*time.Second,
	)
}

// buildSleepScheduler returns the daemon's idle dream-pass scheduler from
// the pre-loaded config. A nil cfg (load failure) degrades to a disabled
// scheduler. The scheduler is always returned non-nil so `guild daemon
// status` can report sleep state; SchedulerConfig.Enabled gates whether it
// ever fires.
func buildSleepScheduler(host *mcp.DaemonHost, cfg *config.Config) *daemon.Scheduler {
	if cfg == nil {
		return daemon.NewScheduler(daemon.SchedulerConfig{Enabled: false, Logger: host.Logger()})
	}
	return daemon.NewScheduler(daemon.SchedulerConfig{
		Enabled: cfg.Sleep.Enabled,
		Idle:    time.Duration(cfg.Sleep.IdleMinutes) * time.Minute,
		Budget:  time.Duration(cfg.Sleep.PassBudgetSeconds) * time.Second,
		Pass:    host.SleepPassRunner(),
		Logger:  host.Logger(),
	})
}

// buildWatchPipeline returns the daemon's watch -> staleness -> renewal
// pipeline from the pre-loaded config. A nil cfg (load failure) degrades
// to a disabled pipeline. The pipeline is always returned non-nil so
// `guild daemon status` can report watcher state; PipelineConfig.Enabled
// (driven by [daemon] watch) gates whether a watcher ever starts. The
// host supplies the roots enumerator and per-event processor so
// internal/daemon stays free of internal/lore, internal/quest, and
// internal/project.
func buildWatchPipeline(host *mcp.DaemonHost, cfg *config.Config) *daemon.Pipeline {
	if cfg == nil {
		return daemon.NewPipeline(daemon.PipelineConfig{Enabled: false, Logger: host.Logger()})
	}
	return daemon.NewPipeline(daemon.PipelineConfig{
		Enabled:  cfg.Daemon.Watch,
		Roots:    host.WatchRoots(),
		Process:  host.WatchProcessor(cfg.Daemon.RenewalCapPerPass),
		Debounce: time.Duration(cfg.Daemon.WatchDebounceMS) * time.Millisecond,
		Logger:   host.Logger(),
	})
}

// ───────────────────────── lifecycle verbs ──────────────────────────

// daemonStatusExitNotRunning is the exit code `guild daemon status`
// uses when no daemon is running: distinct from 0 (running) and 1
// (probe error), mirroring the LSB/systemctl convention where 3 means
// "program is not running". Documented in the command's Long help in
// internal/cli/daemon_cmd.go.
const daemonStatusExitNotRunning = 3

// runDaemonStart is the cobra RunE for `guild daemon start`.
func runDaemonStart(cmd *cobra.Command, _ []string) error {
	res, err := daemon.Start(daemon.StartOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	printStartResult(cmd.OutOrStdout(), res)
	return nil
}

// printStartResult renders a StartResult: shared by start and restart
// so both verbs report a launch identically.
func printStartResult(out io.Writer, res daemon.StartResult) {
	if res.AlreadyRunning {
		fmt.Fprintf(out, "guild daemon: already running pid=%d socket=%s\n", res.PID, res.SocketPath)
	} else {
		fmt.Fprintf(out, "guild daemon: started pid=%d socket=%s\n", res.PID, res.SocketPath)
	}
	if nudge := daemon.FormatVersionDrift(res.DaemonVersion, version); nudge != "" {
		fmt.Fprintln(out, nudge)
	}
}

// runDaemonStop is the cobra RunE for `guild daemon stop`.
func runDaemonStop(cmd *cobra.Command, _ []string) error {
	res, err := daemon.Stop(daemon.StopOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	printStopResult(cmd.OutOrStdout(), res)
	return nil
}

// printStopResult renders a StopResult: shared by stop and restart.
func printStopResult(out io.Writer, res daemon.StopResult) {
	switch {
	case !res.WasRunning:
		fmt.Fprintln(out, "guild daemon: not running")
	case res.Killed:
		fmt.Fprintf(out, "guild daemon: stopped pid=%d (escalated to SIGKILL after grace period)\n", res.PID)
	default:
		fmt.Fprintf(out, "guild daemon: stopped pid=%d\n", res.PID)
	}
}

// runDaemonStatus is the cobra RunE for `guild daemon status`.
func runDaemonStatus(cmd *cobra.Command, _ []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")

	rep, err := daemon.QueryStatus(version, 0)
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}

	out := cmd.OutOrStdout()
	switch {
	case asJSON:
		if err := writeDaemonStatusJSON(out, rep); err != nil {
			return fmt.Errorf("daemon status: %w", err)
		}
	case rep.Running:
		fmt.Fprintf(out, "guild daemon: running pid=%d version=%s uptime=%s sessions=%d embedder=%s socket=%s\n",
			rep.Status.PID, rep.Status.Version, rep.Uptime,
			rep.Status.ActiveSessions, rep.Status.EmbedderState, rep.SocketPath)
		fmt.Fprintln(out, formatWatchLine(rep.Status.Watch))
		fmt.Fprintln(out, daemon.FormatLeasesLine(rep.Status.LeasesReaped))
		for _, line := range daemon.FormatSessionLines(rep.Status.Sessions, time.Now()) {
			fmt.Fprintln(out, line)
		}
		if rep.DriftNudge != "" {
			fmt.Fprintln(out, rep.DriftNudge)
		}
	default:
		fmt.Fprintln(out, "guild daemon: not running")
	}

	if !rep.Running {
		// Distinct exit code for "not running" (documented in Long).
		// os.Exit is safe here: the report is already written and this
		// process holds no state needing teardown; main.go's own error
		// path also exits directly.
		os.Exit(daemonStatusExitNotRunning)
	}
	return nil
}

// formatWatchLine renders the watch pipeline's one-line status under the
// daemon's running line. It reads at a glance: off (opt-out), degraded
// (enabled but no live watcher), or watching N projects with cumulative
// event / signal / renewal counters.
func formatWatchLine(w daemon.WatchStatus) string {
	if !w.Enabled {
		return "  watch: disabled"
	}
	state := "watching"
	if !w.Watching {
		state = "degraded (query-time staleness)"
	}
	line := fmt.Sprintf("  watch: %s projects=%d events=%d signals=%d renewals=%d",
		state, w.ProjectsWatched, w.EventsSeen, w.SignalsRecorded, w.QuestsPosted)
	if w.LastError != "" {
		line += fmt.Sprintf(" last_error=%q", w.LastError)
	}
	return line
}

// daemonStatusView is the --json shape of `guild daemon status`. Keys
// are stable; additions are append-only.
type daemonStatusView struct {
	Running        bool                `json:"running"`
	PID            int                 `json:"pid,omitempty"`
	Version        string              `json:"version,omitempty"`
	SelfVersion    string              `json:"self_version"`
	StartedAt      string              `json:"started_at,omitempty"`
	UptimeSeconds  int64               `json:"uptime_seconds"`
	ActiveSessions int                 `json:"active_sessions"`
	EmbedderState  string              `json:"embedder_state,omitempty"`
	SocketPath     string              `json:"socket_path,omitempty"`
	VersionDrift   bool                `json:"version_drift"`
	Watch          *daemonWatchView    `json:"watch,omitempty"`
	LeasesReaped   int64               `json:"leases_reaped"`
	Sessions       []daemonSessionView `json:"sessions,omitempty"`
}

// daemonSessionView is the --json shape of one live session's presence
// detail. Present only when the daemon is running with live sessions.
type daemonSessionView struct {
	ID            string   `json:"id"`
	Project       string   `json:"project,omitempty"`
	ConnectedAt   string   `json:"connected_at"`
	LastHeartbeat string   `json:"last_heartbeat"`
	HeldQuests    []string `json:"held_quests,omitempty"`
}

// daemonWatchView is the --json shape of the watch pipeline state. Present
// only when the daemon is running (so a not-running report stays minimal).
type daemonWatchView struct {
	Enabled         bool   `json:"enabled"`
	Watching        bool   `json:"watching"`
	ProjectsWatched int    `json:"projects_watched"`
	EventsSeen      int64  `json:"events_seen"`
	SignalsRecorded int64  `json:"signals_recorded"`
	QuestsPosted    int64  `json:"quests_posted"`
	LastError       string `json:"last_error,omitempty"`
}

// writeDaemonStatusJSON emits the one-object JSON form of a status
// report, newline-terminated for line-oriented consumers.
func writeDaemonStatusJSON(out io.Writer, rep daemon.StatusReport) error {
	view := daemonStatusView{
		Running:      rep.Running,
		SelfVersion:  rep.SelfVersion,
		VersionDrift: rep.Drift,
	}
	if rep.Running {
		view.PID = rep.Status.PID
		view.Version = rep.Status.Version
		view.StartedAt = rep.Status.StartedAt.Format(time.RFC3339)
		view.UptimeSeconds = int64(rep.Uptime / time.Second)
		view.ActiveSessions = rep.Status.ActiveSessions
		view.EmbedderState = rep.Status.EmbedderState
		view.SocketPath = rep.SocketPath
		view.LeasesReaped = rep.Status.LeasesReaped
		for _, sess := range rep.Status.Sessions {
			view.Sessions = append(view.Sessions, daemonSessionView{
				ID:            sess.ID,
				Project:       sess.Project,
				ConnectedAt:   sess.ConnectedAt.Format(time.RFC3339),
				LastHeartbeat: sess.LastHeartbeat.Format(time.RFC3339),
				HeldQuests:    sess.HeldQuests,
			})
		}
		w := rep.Status.Watch
		view.Watch = &daemonWatchView{
			Enabled:         w.Enabled,
			Watching:        w.Watching,
			ProjectsWatched: w.ProjectsWatched,
			EventsSeen:      w.EventsSeen,
			SignalsRecorded: w.SignalsRecorded,
			QuestsPosted:    w.QuestsPosted,
			LastError:       w.LastError,
		}
	}
	data, err := json.Marshal(view)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

// runDaemonRestart is the cobra RunE for `guild daemon restart`: stop
// (idempotent) then start, reusing the same helpers as the standalone
// verbs.
func runDaemonRestart(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()

	stopRes, err := daemon.Stop(daemon.StopOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	printStopResult(out, stopRes)

	startRes, err := daemon.Start(daemon.StartOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	printStartResult(out, startRes)
	return nil
}
