package main

import (
	"context"
	"errors"
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
)

// shimProbeTimeout bounds the daemon liveness dial runMCPServe performs
// at startup. With no daemon running the probe costs one failed read of
// ~/.guild/daemon.json and returns without dialing at all; the timeout
// only caps the single unix dial made when a discovery record exists.
const shimProbeTimeout = 250 * time.Millisecond

// wireMCPServe wires the `guild mcp serve` cobra RunE onto
// internal/cli's mcpServeCmd placeholder. Factored into its own file
// (and its own init) per QUEST-12 scope boundary: root.go must stay
// MCP-agnostic, so the wiring lives here where the mcp + cli packages
// are already linked by the main binary. The cli package exposes
// SetMCPServeRunE as the single attachment seam — see
// internal/cli/root.go.
func wireMCPServe() {
	cli.SetMCPServeRunE(runMCPServe)
}

// init runs at binary startup, before cobra.Execute. Wiring via init
// means the cmd/guild build is the only place that assembles the MCP
// handler; tests that import internal/cli directly (see root_test.go)
// keep seeing the [errNotImplemented] placeholder and don't pull in
// the SDK.
func init() {
	wireMCPServe()
	// Wire the ldflags-stamped version into the MCP layer so
	// guild_session_start can emit an upgrade nudge when appropriate.
	mcp.SetBinaryVersion(version)
}

// runMCPServe is the cobra RunE for `guild mcp serve`. Uses a
// [signal.NotifyContext] on SIGINT + SIGTERM so Ctrl-C and `kill <pid>`
// cleanly cancel the server ctx and let [mcp.Serve] tear down the stdio
// transport.
//
// Before serving in-process it probes once for a live guild daemon
// (ADR-005 shim probe). A live, version-matched daemon turns this
// process into a dumb byte pipe to the daemon socket; anything else
// reaches [mcp.Serve] exactly as it always has. The probe is the ONLY
// addition on the no-daemon path: no output, no files written, no
// reordered side effects, so harness configs and behavior without a
// daemon stay byte-identical.
//
// The parent ctx (context.Background) is used rather than
// cmd.Context(): cobra's default context is uncancelled at this point
// and we want ONLY signal-driven cancellation for the server loop.
// Tests that exercise mcp.Serve directly build their own ctx with
// their own cancel function.
func runMCPServe(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// stop is documented to be idempotent; deferred release ensures
	// the signal handler is unregistered even if Serve returns early
	// with a protocol error (and not via ctx-cancel).
	defer stop()

	if done, err := tryDaemonShim(ctx, version, os.Stderr); done {
		return err
	}

	if err := mcp.Serve(ctx); err != nil {
		// Surface ONLY the error (no stderr banner); cobra's
		// SilenceUsage handles the usage print. The error message
		// travels up to main.go which prints "error: <msg>" once.
		return fmt.Errorf("mcp serve: %w", err)
	}
	return nil
}

// tryDaemonShim probes for a live daemon and, when one matches this
// binary's version, serves the whole stdio session as a pipe to its
// socket. Returns done=true when the shim owned the session (RunShim
// completed or failed unrecoverably); done=false means the caller must
// run the in-process server, with stdio guaranteed untouched.
//
// Probe outcomes:
//
//   - not_running: when autostart is enabled (the default), the first
//     such shim spawns a daemon under an exclusive lock and pipes to
//     it; concurrent shims wait for that winner. When autostart is
//     disabled (GUILD_NO_DAEMON=1, --no-daemon, or [daemon] autostart =
//     false) this is the byte-identical no-daemon path: no spawn, no
//     lock file, no output (ADR-005 hard invariant).
//   - running_mismatch: exactly one nudge line on errOut (stderr in
//     production; never stdout, which is the protocol channel), then
//     in-process. Never autostarts: a daemon IS running, just skewed.
//   - running_match: pipe mode via daemon.RunShim, with mcp.ServeIO
//     wired as the mid-session crash fallback.
//
// Probe errors are environmental (unresolvable home, unreadable
// discovery file) and are deliberately swallowed: the in-process
// server is always the safe answer, and it will surface the same
// environment problem with its own, better-established error paths.
func tryDaemonShim(ctx context.Context, selfVersion string, errOut io.Writer) (done bool, err error) {
	res, live, perr := daemon.Probe(selfVersion, shimProbeTimeout)
	if perr != nil {
		return false, nil
	}

	switch res {
	case daemon.RunningMismatch:
		fmt.Fprintln(errOut, daemon.FormatSkewNudge(live.Version, selfVersion))
		return false, nil
	case daemon.RunningMatch:
		return pipeToDaemon(ctx, selfVersion, live)
	default: // daemon.NotRunning
		if !autostartEnabled() {
			// Opt-out path: byte-identical to a build without daemon
			// support. No lock file, no spawn, no output.
			return false, nil
		}
		res, live, aerr := daemon.Autostart(daemon.AutostartOptions{SelfVersion: selfVersion})
		if aerr != nil {
			// Spawn or environment failure. One diagnostic line on stderr
			// (never stdout, the protocol channel), then serve in-process:
			// correctness never depends on the daemon.
			shimLogger().Warn("guild daemon autostart failed; continuing in-process", "err", aerr)
			return false, nil
		}
		if res == daemon.NotRunning {
			// No daemon came up within the budget (or autostart is
			// unsupported on this platform): serve in-process, silently.
			return false, nil
		}
		if res == daemon.RunningMismatch {
			// A concurrent winner spawned a skewed daemon: nudge once and
			// fall through, same as a probe-time mismatch.
			fmt.Fprintln(errOut, daemon.FormatSkewNudge(live.Version, selfVersion))
			return false, nil
		}
		return pipeToDaemon(ctx, selfVersion, live)
	}
}

// autostartEnabled resolves the daemon.autostart knob through the same
// layered config the MCP server loads (env + TOML are the operative
// layers; the server passes nil flags). On any load error it defaults
// to false: an unreadable config must never silently start a daemon, and
// the in-process server will surface the config problem with its own
// error paths.
func autostartEnabled() bool {
	cfg, err := config.Load(nil)
	if err != nil {
		return false
	}
	return cfg.Daemon.Autostart
}

// pipeToDaemon runs the stdio session as a dumb pipe to live's socket,
// with mcp.ServeIO wired as the mid-session crash fallback. Returns
// done=false (stdio untouched) when the daemon vanished before any
// stdio byte was consumed, so the caller serves in-process cleanly.
func pipeToDaemon(ctx context.Context, selfVersion string, live daemon.Discovery) (done bool, err error) {
	cwd, werr := os.Getwd()
	if werr != nil {
		// Without a cwd the daemon would reject the preamble; the
		// in-process server handles a missing cwd gracefully instead.
		return false, nil
	}

	err = daemon.RunShim(ctx, daemon.ShimConfig{
		SocketPath: live.SocketPath,
		Preamble:   daemon.ShimPreamble{Version: selfVersion, CWD: cwd, PID: os.Getpid()},
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Fallback:   mcp.ServeIO,
		Logger:     shimLogger(),
	})
	if errors.Is(err, daemon.ErrShimUnavailable) {
		// The daemon vanished between probe and dial, before any stdio
		// byte was consumed: serve in-process as if it was never there.
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("mcp serve: %w", err)
	}
	return true, nil
}

// shimLogger reports shim lifecycle warnings (daemon lost, re-dial,
// in-process fallback) on stderr. Warn level keeps routine pipe
// operation completely silent; stdout is never involved.
func shimLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
