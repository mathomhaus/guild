package cli

import "github.com/spf13/cobra"

// Daemon lifecycle verbs: start, stop, status, restart.
//
// Like daemonRunCmd in root.go, these are placeholders: the command
// definitions (and therefore the generated CLI docs) live here, while
// the real RunE handlers are attached by cmd/guild/daemon.go via
// SetDaemonLifecycleRunEs. The boundary keeps internal/cli free of the
// daemon assembly the same way SetMCPServeRunE keeps it free of the
// MCP SDK; tests that import internal/cli without cmd/guild keep the
// errNotImplemented placeholders.

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "start the guild daemon in the background",
	Long: `Starts the guild daemon as a detached background process.

The daemon is re-executed from this binary as 'guild daemon run' in
its own session: it survives this command (and the shell) exiting,
reads from the null device, and writes its log to ~/.guild/daemon.log.
The log rotates on every start; the previous generation is kept at
~/.guild/daemon.log.old.

The command waits a few seconds for the daemon to become ready
(discovery file written, socket accepting), then prints the daemon's
pid and socket path. Idempotent: when a daemon is already running it
reports that daemon's pid and exits 0, with a one-line version-drift
nudge if the running daemon was built from a different version.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return errNotImplemented
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "stop the running guild daemon",
	Long: `Stops the running guild daemon.

Reads the daemon's pid from ~/.guild/daemon.json, sends SIGTERM, and
waits for the process to exit. A daemon still alive after the grace
period is killed with SIGKILL. The daemon's socket and discovery file
are gone on success (removed on its behalf when the daemon could not
run its own cleanup).

Idempotent: exits 0 both when a daemon was stopped and when none was
running.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return errNotImplemented
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "show whether the guild daemon is running",
	Long: `Reports the running daemon's pid, version, uptime, active session
count, and embedder state on one line, with a second nudge line when
the daemon's version differs from this binary's. --json emits a single
machine-readable JSON object instead.

Exit status:

  0  a daemon is running
  1  the status probe failed with an unexpected error
  3  no daemon is running`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return errNotImplemented
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "restart the guild daemon (stop, then start)",
	Long: `Restarts the guild daemon: stop (if one is running) followed by start.

The main use is picking up a new binary after an upgrade: a long-lived
daemon keeps serving the code it started with, and 'guild daemon
status' nudges when its version drifts from this binary's. Exits 0
with the new daemon's pid on success.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return errNotImplemented
	},
}

// SetDaemonLifecycleRunEs installs the real handlers for `guild daemon
// start|stop|status|restart`. Same seam pattern as SetDaemonRunRunE:
// cmd/guild owns the internal/daemon import and attaches the RunEs at
// binary init, so internal/cli never depends on the daemon assembly.
func SetDaemonLifecycleRunEs(start, stop, status, restart func(*cobra.Command, []string) error) {
	daemonStartCmd.RunE = start
	daemonStopCmd.RunE = stop
	daemonStatusCmd.RunE = status
	daemonRestartCmd.RunE = restart
}

func init() {
	daemonStatusCmd.Flags().Bool("json", false,
		"emit one machine-readable JSON object instead of the human line")
	daemonCmd.AddCommand(daemonStartCmd, daemonStopCmd, daemonStatusCmd, daemonRestartCmd)
}
