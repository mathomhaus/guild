package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/install"
)

// `guild daemon install` / `guild daemon uninstall`: the daemon's
// persistence tier: a launchd agent on macOS, a systemd user unit on
// Linux.
//
// Unlike the lifecycle verbs in daemon_cmd.go, these need no cmd/guild
// seam: the implementation lives in internal/install (already a CLI
// dependency via `guild init` and `guild mcp install`) and touches
// neither the MCP SDK nor the daemon assembly, so the RunEs attach
// directly here.

var daemonInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "install the guild daemon as a login service (launchd agent / systemd user unit)",
	Long: `Installs the guild daemon as an always-on login service:

  macOS  writes ~/Library/LaunchAgents/com.mathomhaus.guild.daemon.plist
         and loads it via 'launchctl bootstrap gui/$UID' (falling back to
         the legacy 'launchctl load -w' on older systems)
  Linux  writes ~/.config/systemd/user/guild-daemon.service (honoring
         $XDG_CONFIG_HOME) and runs 'systemctl --user daemon-reload'
         followed by 'systemctl --user enable --now'

The unit executes this binary's resolved absolute path with 'daemon
run'; re-run install after moving the binary. A warning (not an error)
is printed when the resolved path looks transient (temp or go-build
directory), since the unit would break the next time that directory is
cleaned.

The service manager owns the daemon process from here: it starts it at
login and keeps it alive, so 'guild daemon stop' only pauses it until
the next restart. Use 'guild daemon uninstall' to remove the service
permanently. The daemon writes its own log to ~/.guild/daemon.log; the
unit captures no stdout/stderr of its own.

Idempotent: repeat installs re-render the unit file in place and reload
it, never creating duplicates. When launchctl/systemctl is not on PATH
the rendered unit and manual load instructions are printed instead and
the command still succeeds.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if _, err := install.DaemonInstall(install.DaemonUnitOptions{Out: cmd.OutOrStdout()}); err != nil {
			return fmt.Errorf("daemon install: %w", err)
		}
		return nil
	},
}

var daemonUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "remove the guild daemon login service",
	Long: `Removes the login service created by 'guild daemon install': asks the
service manager to drop the unit ('launchctl bootout' on macOS,
'systemctl --user disable --now' on Linux) and deletes the unit file.

Idempotent: exits 0 when no unit is installed or the unit was never
loaded. When launchctl/systemctl is not on PATH the unit file is still
removed and manual unload instructions are printed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if _, err := install.DaemonUninstall(install.DaemonUnitOptions{Out: cmd.OutOrStdout()}); err != nil {
			return fmt.Errorf("daemon uninstall: %w", err)
		}
		return nil
	},
}

func init() {
	daemonCmd.AddCommand(daemonInstallCmd, daemonUninstallCmd)
}
