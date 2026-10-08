// Package cli assembles the cobra command tree for the guild binary.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var errNotImplemented = errors.New("not yet implemented.")

// buildVersion, buildCommit, buildDate hold the ldflags-stamped values
// injected by cmd/guild/main.go via SetVersion. Defaults are "dev" / ""
// so that `go build` without ldflags still produces a usable binary.
var buildVersion = "dev"
var buildCommit = ""
var buildDate = ""

// SetVersion wires the build-time stamp values from cmd/guild/main.go
// into the CLI layer. Must be called before Execute().
func SetVersion(version, commit, date string) {
	buildVersion = version
	buildCommit = commit
	buildDate = date
}

var rootCmd = &cobra.Command{
	Use:   "guild",
	Short: "persistent cognition for AI agents — task + knowledge lifecycle",
	Long: `guild bundles three modes in one static binary:

  guild lore <verb>    knowledge lifecycle (inscribe, appraise, study, ...)
  guild quest <verb>   task lifecycle   (post, accept, clear, ...)
  guild mcp serve      MCP stdio server for AI agents

The lore, quest, and MCP surfaces share one SQLite-backed store under
~/.guild/. See https://github.com/mathomhaus/guild for docs.

Next step — if you haven't yet:

  guild mcp install     register guild with your AI agent(s)
  cd <your project> && guild init    scaffold AGENTS.md for this repo

Then open the project in your agent and call
mcp__guild__guild_session_start(project="<dir-name>").

Environment variables:

  GUILD_NO_UPDATE_CHECK=1   disable the upgrade-available nudge on stderr
  GUILD_AGENT=1             force agent mode (one JSON envelope per verb
                            on stdout); =0 forces it off. Auto-detected
                            for known agent harness shells; see --agent`,
	SilenceUsage: true,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "print guild version information",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("guild version=%s commit=%s date=%s\n", buildVersion, buildCommit, buildDate)
	},
}

var loreCmd = &cobra.Command{
	Use:   "lore",
	Short: "knowledge lifecycle (read/write/decay/supersede)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var questCmd = &cobra.Command{
	Use:   "quest",
	Short: "task lifecycle (post/accept/journal/clear/coordinate)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "MCP server subcommands",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "run the guild MCP stdio server for AI agents",
	RunE: func(_ *cobra.Command, _ []string) error {
		return errNotImplemented
	},
}

// SetMCPServeRunE installs the real `guild mcp serve` handler. Exposed
// as a seam so cmd/guild (which owns the internal/mcp import) can
// attach the RunE without internal/cli depending on the SDK. Tests that
// import internal/cli without cmd/guild keep seeing the errNotImplemented
// placeholder and do NOT pull in the MCP SDK surface.
func SetMCPServeRunE(run func(*cobra.Command, []string) error) {
	mcpServeCmd.RunE = run
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "guild daemon subcommands",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "run the guild daemon in the foreground (MCP sessions over a unix socket)",
	Long: `Runs the guild daemon in the foreground until interrupted.

The daemon listens on a unix socket under ~/.guild/ and serves the same
MCP tool surface as 'guild mcp serve', one session per connection. All
sessions share a single embedder and a single process writing to the
guild databases. A discovery file (~/.guild/daemon.json) is written on
start and removed on exit; SIGINT/SIGTERM stop the daemon cleanly.

Optional: agents work identically without the daemon. It only changes
how sessions are served, never what they can do.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		return errNotImplemented
	},
}

// SetDaemonRunRunE installs the real `guild daemon run` handler. Same
// seam pattern as SetMCPServeRunE: cmd/guild owns the internal/daemon +
// internal/mcp imports and attaches the RunE at init, so internal/cli
// never depends on the MCP SDK or the daemon listener.
func SetDaemonRunRunE(run func(*cobra.Command, []string) error) {
	daemonRunCmd.RunE = run
}

// upgradeNudgeFn is the optional hook called by the PersistentPreRun on
// every CLI invocation. cmd/guild/main.go injects the real implementation
// via SetUpgradeNudge. When nil, no nudge is emitted (the default in tests
// that import internal/cli without cmd/guild).
var upgradeNudgeFn func() string

// SetUpgradeNudge installs the upgrade-check function called on every CLI
// command. fn must return a non-empty string when a newer release is
// available, and "" otherwise. The function is called in PersistentPreRun
// so it fires for every subcommand. Nudge output goes to stderr so that
// stdout consumers (scripted pipelines) are never polluted.
//
// Exposed as a seam so cmd/guild can inject the real implementation (which
// imports internal/release) without internal/cli depending on that package.
func SetUpgradeNudge(fn func() string) {
	upgradeNudgeFn = fn
}

func init() {
	// PersistentPreRun fires before every subcommand. We use it to emit
	// an upgrade-available nudge when a newer guild release exists and
	// stderr is a TTY. The isatty check happens here (not in SetUpgradeNudge)
	// so that cmd/guild can inject a context-free fn that returns just the string.
	rootCmd.PersistentPreRun = func(_ *cobra.Command, _ []string) {
		if upgradeNudgeFn == nil {
			return
		}
		if !isatty.IsTerminal(os.Stderr.Fd()) && !isatty.IsCygwinTerminal(os.Stderr.Fd()) {
			return
		}
		if msg := upgradeNudgeFn(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	}

	// --no-emoji is a persistent global flag so every subcommand inherits
	// it. config.flagLayer reads "no-emoji" from the merged FlagSet, so
	// declaring it here (rootCmd.PersistentFlags) makes it available to
	// every subcommand via cmd.Flags() after cobra merges the flag sets.
	// GUILD_NO_EMOJI=1 is the env equivalent (wired in config.envLayer).
	rootCmd.PersistentFlags().Bool("no-emoji", false, "plain-text ASCII output (accessibility / dumb terminals)")

	// --verbose / -v is a persistent global flag: the conventional UNIX
	// meaning of -v is "verbose", not "version" (QUEST-10). Subcommands
	// read cmd.Flags().GetBool("verbose") to expand their output.
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "enable verbose output")

	// --agent is a persistent global flag: registry-generated verbs emit
	// one machine-readable JSON envelope per invocation instead of the
	// human rendering. Auto-detected from known agent-harness env markers
	// (or GUILD_AGENT=1); an explicit --agent=false forces human output.
	// The runtime check lives in internal/command (agentModeActive) next
	// to the envelope writer. Note: quest journal/orders/campfire/summon
	// keep their historical local string --agent (agent identity), which
	// shadows this flag; agent mode reaches them via the env path.
	rootCmd.PersistentFlags().Bool("agent", false,
		"machine-readable JSON output for coding agents (auto-detected; --agent=false forces human output)")

	// --module / --disable-module are the CLI flag layer for ADR-006
	// Phase 3 capability-module toggles. config.flagLayer reads both from
	// the merged FlagSet, so declaring them as persistent globals makes
	// them available to every subcommand. --module NAME=BOOL is the
	// explicit toggle (repeatable); --disable-module NAME forces a module
	// off (repeatable), mirroring --no-daemon. The env equivalents are
	// GUILD_MODULE_<NAME> and GUILD_NO_<NAME> (wired in config.envLayer).
	rootCmd.PersistentFlags().StringArray("module", nil,
		"toggle a capability module: --module NAME=true|false (repeatable)")
	rootCmd.PersistentFlags().StringArray("disable-module", nil,
		"disable a capability module by name (repeatable)")

	// --version retains its long spelling. Its short flag is -V so that
	// -v stays reserved for --verbose (see above). `guild version` (the
	// subcommand) is the portable form that works everywhere.
	rootCmd.Flags().BoolP("version", "V", false, "print version information and exit")
	rootCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		showVer, _ := cmd.Flags().GetBool("version")
		if showVer {
			fmt.Printf("guild version=%s commit=%s date=%s\n", buildVersion, buildCommit, buildDate)
			return nil
		}
		return cmd.Help()
	}

	// Bind the module-registry verbs onto questCmd / loreCmd at this exact
	// point — AFTER quest.go/lore_*.go init() set up the parents and their
	// persistent flags (those files sort before root.go in init order), and
	// BEFORE the AddCommand below attaches the groups to rootCmd so the root
	// persistent --agent Bool is not yet inherited into the per-verb
	// flagsets (which would clash with quest journal/orders/campfire/summon's
	// local string --agent and panic with "--agent flag redefined"). Calling
	// it explicitly here pins that load-bearing window by call order instead
	// of by the "register_modules.go" filename sorting between quest.go and
	// root.go, so a renamed or added cli file cannot silently break it.
	setupModuleVerbs()

	mcpCmd.AddCommand(mcpServeCmd)
	daemonCmd.AddCommand(daemonRunCmd)
	rootCmd.AddCommand(loreCmd, questCmd, mcpCmd, daemonCmd, versionCmd)
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

// Root returns the configured cobra root command. Exposed for tooling
// (cmd/docgen walks the tree to emit docs/generated/cli.md).
func Root() *cobra.Command {
	return rootCmd
}
