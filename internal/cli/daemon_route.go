package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/module"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
)

// This file wires ADR-005's CLI routing: when a version-matched guild
// daemon is up, registry-generated terminal verbs run their Handler
// INSIDE the daemon (single writer) over the JSON-exec RPC, while all
// rendering stays in this process and byte-identical. Both halves live
// here:
//
//   - client half: the probe-once route state and the
//     command.Deps.ExecRemote transport installed by
//     buildCLICommandDeps / buildCLILoreDeps.
//   - daemon half: NewDaemonExecHandler, the dispatch table built from
//     the same registry specs the CLI binds, with CLI-equivalent Deps
//     resolved against the CLIENT's cwd. cmd/guild wires it into
//     daemon.Config.Exec.
//
// Correctness never depends on the daemon: every transport failure
// lands back on the local Handler.

// routeProbeTimeout bounds the liveness dial inside the routing probe.
// Mirrors the daemon's own startup probe budget.
const routeProbeTimeout = time.Second

// daemonRouteState is the probe-once outcome for this process.
type daemonRouteState struct {
	// socket is the live, version-matched daemon's socket path; empty
	// means every verb executes locally.
	socket string
	// nudge is the one-line version-skew notice (RunningMismatch only),
	// emitted at most once, to stderr, only when stderr is a TTY.
	nudge string
}

// Routing seams. The probe and TTY check are injectable so tests stay
// hermetic (a developer's live daemon must never receive test traffic),
// and the nudge writer is capturable.
var (
	routeOnce      sync.Once
	routeState     daemonRouteState
	routeNudgeOnce sync.Once

	// routeProbeFn defaults to the real discovery probe. The probe costs
	// one daemon.json stat when no daemon has ever run; the only
	// daemon-down overhead this file adds.
	routeProbeFn = daemon.Probe

	routeIsTTYFn = func() bool {
		return isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())
	}
	routeStderr io.Writer = os.Stderr
)

// resetDaemonRouteForTest clears the probe-once route state so tests
// can exercise multiple probe outcomes in one process. Test-only, like
// resetQuestFlagState.
func resetDaemonRouteForTest() {
	routeOnce = sync.Once{}
	routeState = daemonRouteState{}
	routeNudgeOnce = sync.Once{}
}

// resolveDaemonRoute probes for a routable daemon exactly once per
// process. It runs lazily on the first registry-verb invocation (never
// at package init: the ldflags-stamped version is only wired in via
// SetVersion after init, and non-verb invocations like `guild --help`
// should not pay even the stat).
func resolveDaemonRoute() daemonRouteState {
	routeOnce.Do(func() {
		res, d, err := routeProbeFn(buildVersion, routeProbeTimeout)
		if err != nil {
			return // environmental failure → local execution, no noise
		}
		switch res {
		case daemon.RunningMatch:
			routeState.socket = d.SocketPath
		case daemon.RunningMismatch:
			routeState.nudge = daemon.FormatSkewNudge(d.Version, buildVersion)
		case daemon.NotRunning:
			// zero state: local execution.
		}
	})
	return routeState
}

// errDaemonNotRouted tells command.dispatchHandler to run the local
// Handler. It is the daemon-down common case, not a failure.
var errDaemonNotRouted = errors.New("cli: no routable daemon")

// remoteExecViaDaemon implements command.Deps.ExecRemote. Transport
// failures retry once and then surface as plain errors (the adapter
// falls back to the local Handler); daemon-side Handler failures come
// back as *command.RemoteHandlerError and are final.
func remoteExecViaDaemon(ctx context.Context, req command.RemoteExecRequest) (json.RawMessage, error) {
	st := resolveDaemonRoute()
	if st.nudge != "" {
		routeNudgeOnce.Do(func() {
			if routeIsTTYFn() {
				fmt.Fprintln(routeStderr, st.nudge)
			}
		})
	}
	if st.socket == "" {
		return nil, errDaemonNotRouted
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("cli: daemon route: getwd: %w", err)
	}
	pre := daemon.ExecPreamble{
		Tool:    req.Tool,
		Version: buildVersion,
		CWD:     cwd,
		PID:     os.Getpid(),
		NoEmoji: req.NoEmoji,
	}
	res, herr, err := daemon.ExecCall(ctx, st.socket, pre, req.Args)
	if err != nil {
		// One retry: transient conn drops (daemon restarting, accept
		// backlog) are worth a second dial before giving up on routing.
		//
		// At-least-once window: a transport error does not prove the
		// daemon never ran the Handler. If the failure hit AFTER the
		// request line was delivered (response lost mid-read, conn reset
		// post-write), the first attempt may have committed and this
		// retry, or the local fallback behind it, re-executes a mutating
		// verb. The spec's retry-once rule accepts that duplicate-write
		// window; exec-call framing has no idempotency token to dedupe
		// on, so we trade exactness for daemon-restart resilience.
		res, herr, err = daemon.ExecCall(ctx, st.socket, pre, req.Args)
		if err != nil {
			return nil, err
		}
	}
	if herr != nil {
		return nil, &command.RemoteHandlerError{
			Message:     herr.Message,
			Hint:        herr.Hint,
			Narration:   herr.Narration,
			NarrationOK: herr.NarrationOK,
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Lazy per-process embed wiring
// ---------------------------------------------------------------------------

// cliQuestEmbedSource defers wireQuestEmbedDeps until a quest verb
// actually executes its Handler in THIS process. When the daemon serves
// the verb, the resolver is never consulted and the per-process index
// build (the accepted 50-200 ms startup cost, QUEST-259) is skipped
// entirely; the daemon's shared embedder serves the search instead.
// Satisfies quest's questEmbedResolver seam.
type cliQuestEmbedSource struct {
	once sync.Once
	deps *quest.QuestEmbedDeps
}

// ResolveQuestEmbedDeps wires lazily on first use. The wiring runs
// under its own bounded 15s budget rather than the request ctx, a
// deliberate cap inherited from the eager-wiring era.
//
//nolint:contextcheck // see budget note above
func (s *cliQuestEmbedSource) ResolveQuestEmbedDeps(context.Context) *quest.QuestEmbedDeps {
	s.once.Do(func() { s.deps = wireQuestEmbedDeps() })
	return s.deps
}

// cliLoreEmbedSource is the lore-side sibling: wireCLIEmbedDeps on
// first local Handler use. Satisfies lore's embedResolver seam.
type cliLoreEmbedSource struct {
	once sync.Once
	deps *lore.EmbedDeps
}

// ResolveEmbedDeps wires lazily on first use, under the wiring's own
// bounded 10s budget rather than the request ctx (same rationale as
// cliQuestEmbedSource).
//
//nolint:contextcheck // see budget note above
func (s *cliLoreEmbedSource) ResolveEmbedDeps(context.Context) *lore.EmbedDeps {
	s.once.Do(func() { s.deps = wireCLIEmbedDeps() })
	return s.deps
}

// ---------------------------------------------------------------------------
// Daemon half: the JSON-exec dispatch table
// ---------------------------------------------------------------------------

// cliRegistryBoundVerbs accumulates the wire name of every registry
// spec the terminal CLI binds (bindRegistryVerb / bindLoreRegistryVerb),
// in bind order, exec-exempt verbs included. Populated during package
// init, read-only afterwards. The routing tests diff it against the
// daemon-side dispatch table so a new bind cannot silently miss
// NewDaemonExecHandler (or vice versa).
var cliRegistryBoundVerbs []string

// NewDaemonExecHandler builds the daemon-side dispatcher for the
// JSON-exec RPC over the same registry specs the terminal CLI binds.
// questEmbed / loreEmbed carry the daemon process's SHARED embed
// providers (opaque command.Deps.Embed values from internal/mcp's
// DaemonHost); nil falls back to BM25-only exactly like an unwired CLI.
func NewDaemonExecHandler(questEmbed, loreEmbed any) daemon.ExecHandler {
	reg := newDaemonExecRegistry(questEmbed, loreEmbed)

	return func(ctx context.Context, req daemon.ExecRequest) (json.RawMessage, *daemon.ExecHandlerError, error) {
		res, herr, err := reg.Exec(ctx, req.Tool, req.CWD, req.NoEmoji, req.Args)
		if herr != nil {
			return nil, &daemon.ExecHandlerError{
				Message:     herr.Message,
				Hint:        herr.Hint,
				Narration:   herr.Narration,
				NarrationOK: herr.NarrationOK,
			}, nil
		}
		return res, nil, err
	}
}

// newDaemonExecRegistry builds the dispatch table behind
// NewDaemonExecHandler. Split out so the routing tests can diff the
// registered names against cliRegistryBoundVerbs.
//
// ADR-006 Phase 3 cleanup: the daemon-side exec table is no longer a third
// hand-list. It is derived from the SAME module registry the CLI binds
// from — for each enabled module the loop resolves that module's exec Deps
// builder (questDeps vs loreDeps, keyed by name, mirroring
// cliBindTargetForModule) and calls BindExec on every Command the module
// contributes. BindExec routes through RegisterExec, so exec-exempt verbs
// (quest_orders) are skipped exactly as before, and the same cliBespokeVerbs
// the CLI loop skips (lore_appraise/lore_study, which have hand-rolled
// cobra commands and were never in the exec list; quest_clear, MCPOnly and
// never CLI-bound) are skipped here too. The result is the identical verb
// SET the old explicit list produced, which TestDaemonRoute_*
// (diffVerbSets against cliRegistryBoundVerbs) pins.
func newDaemonExecRegistry(questEmbed, loreEmbed any) *command.ExecRegistry {
	reg := command.NewExecRegistry()

	questDeps := func(_ context.Context, cwd string) command.Deps {
		d := command.Deps{
			OpenDB:      openQuestDB,
			ResolveProj: resolveProjAt(cwd, openQuestDB),
			Now:         time.Now,
			OpenLoreDB:  openLoreDB,
			// quest post --spec inscribes a kind=decision lore entry, so
			// routed execs honor the configured decay windows exactly like
			// local runs. config.Load resolves the per-project layer
			// against this daemon's cwd, matching the MCP register wiring
			// in the same process.
			LoreValidDays: cliLoreValidDays,
		}
		if questEmbed != nil {
			d.Embed = questEmbed
		}
		return d
	}
	loreDeps := func(_ context.Context, cwd string) command.Deps {
		d := command.Deps{
			OpenDB:      openLoreDB,
			OpenQuestDB: openQuestDB,
			ResolveProj: resolveProjAt(cwd, openLoreDB),
			Now:         time.Now,
			// Same decay-window threading as questDeps: lore inscribe,
			// update, and catalog import stamp validity daemon-side.
			LoreValidDays: cliLoreValidDays,
		}
		if loreEmbed != nil {
			d.Embed = loreEmbed
		}
		return d
	}

	// execDepsForModule mirrors cliBindTargetForModule but yields the
	// daemon-side exec DepsBuilder, so the exec table and the CLI bind the
	// SAME modules' verbs with the SAME quest/lore split. ok=false means
	// the module contributes no routable verbs (session).
	execDepsForModule := func(name string) (command.DepsBuilder, bool) {
		switch name {
		case "lore":
			return loreDeps, true
		case "quest":
			return questDeps, true
		default:
			return nil, false
		}
	}

	for _, m := range module.Enabled(cliModuleEnabledPredicate()) {
		deps, ok := execDepsForModule(m.Name())
		if !ok {
			continue
		}
		for _, cmd := range m.Commands() {
			// Skip the same verbs the CLI loop skips so the exec set
			// matches cliRegistryBoundVerbs exactly: bespoke (appraise/
			// study, hand-rolled cobra) and quest_clear (MCPOnly, never
			// CLI-bound). BindExec additionally skips exec-exempt verbs
			// (quest_orders) on its own.
			if cliBespokeVerbs[cmd.WireName()] {
				continue
			}
			cmd.BindExec(reg, deps)
		}
	}

	return reg
}

// resolveProjAt mirrors the local CLI's ResolveProj (cwd + git, no
// silent fallback, same hint wrapping) but anchored on the CLIENT's
// cwd: the daemon must resolve the project the caller's shell is in,
// never its own working directory.
func resolveProjAt(cwd string, open func(ctx context.Context) (*sql.DB, error)) func(ctx context.Context, argProject string) (string, error) {
	return func(ctx context.Context, argProject string) (string, error) {
		db, err := open(ctx)
		if err != nil {
			return "", err
		}
		defer func() { _ = db.Close() }()
		r := project.Resolver{Getwd: func() (string, error) { return cwd, nil }}
		p, err := r.Resolve(ctx, db, strings.TrimSpace(argProject))
		if err != nil {
			return "", wrapResolveHint(err)
		}
		return p.ID, nil
	}
}
