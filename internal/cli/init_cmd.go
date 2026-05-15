package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/install"
)

var initCmd = &cobra.Command{
	Use:     "init",
	Short:   "scaffold AGENTS.md and register this repo with guild",
	GroupID: "core",
	Long: `guild init — per-project scaffold

Run inside a git repository. Detects the project name, shows a plan
(register in ~/.guild/, create-or-merge AGENTS.md, MCP registration hint),
then prompts [Y/n] before acting. Accepts all defaults without prompting
when stdin is not a TTY (piped / CI invocation).

Flags:
  --yes             accept all defaults, no prompts
  --dry-run         show the plan without executing any changes
  --print-agents-md emit only the AGENTS.md snippet to stdout (for piping)`,

	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		// Retired flags — hard removal, no deprecation period.
		for _, old := range []string{"write", "merge", "force"} {
			if f := cmd.Flags().Lookup(old); f != nil && f.Changed {
				switch old {
				case "write":
					return fmt.Errorf("--write was removed. Run `guild init` (interactive) or `guild init --yes`")
				case "merge":
					return fmt.Errorf("--merge was removed. Run `guild init` — it detects an existing AGENTS.md and appends")
				case "force":
					return fmt.Errorf("--force was removed. Delete AGENTS.md manually and re-run `guild init`")
				}
			}
		}

		yes, _ := cmd.Flags().GetBool("yes")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		printAgentsMD, _ := cmd.Flags().GetBool("print-agents-md")

		// Non-TTY stdin auto-switches to --yes mode.
		if !yes && !install.IsInteractiveTTYStdin() {
			yes = true
		}

		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("guild init: resolve cwd: %w", err)
		}

		opts := install.InitOptions{
			Yes:           yes,
			DryRun:        dryRun,
			PrintAgentsMD: printAgentsMD,
			Out:           os.Stdout,
			In:            os.Stdin,
		}

		if _, err := install.Init(ctx, cwd, opts); err != nil {
			return fmt.Errorf("guild init: %w", err)
		}
		return nil
	},
}

func init() {
	// Retired flags: registered so cobra does not reject them as unknown;
	// RunE checks .Changed and returns a clean error with a migration hint.
	initCmd.Flags().Bool("write", false, "")
	initCmd.Flags().Bool("merge", false, "")
	initCmd.Flags().Bool("force", false, "")
	_ = initCmd.Flags().MarkHidden("write")
	_ = initCmd.Flags().MarkHidden("merge")
	_ = initCmd.Flags().MarkHidden("force")

	initCmd.Flags().Bool("yes", false, "accept all defaults without prompting")
	initCmd.Flags().Bool("dry-run", false, "show what would happen without making changes")
	initCmd.Flags().Bool("print-agents-md", false, "emit only the AGENTS.md template snippet to stdout")
	rootCmd.AddCommand(initCmd)
}
