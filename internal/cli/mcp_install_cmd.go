package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/install"
)

var mcpInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "print the recommended MCP registration command for each detected agent client",
	Long: `guild mcp install — delegate MCP registration to each client's official CLI

Detects which MCP clients are present on this machine (Claude Code, Cursor,
Codex) and prints the recommended command to register guild with each:

  guild binary: /usr/local/bin/guild

  Detected agent clients:
    ✓ Claude Code
    ✗ Cursor  (not detected)

  Run the command for each agent you use:

    # Claude Code
    claude mcp add guild --scope user -- /usr/local/bin/guild mcp serve

guild never writes client config files directly — it delegates to each
client's official CLI so you keep full control.

Flags:
  --run           shell out to each detected client's CLI (prompts per command)
  --run --yes     shell out without prompting
  --update        re-register clients whose configured path differs from the
                  running binary (#61)
  --force         re-register every detected client unconditionally; implies
                  --update
  --print-config  print only the JSON snippet for manual paste (no detection)
  --skill         (not yet implemented) install Claude Code skill`,

	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		printCfg, _ := cmd.Flags().GetBool("print-config")
		run, _ := cmd.Flags().GetBool("run")
		yes, _ := cmd.Flags().GetBool("yes")
		update, _ := cmd.Flags().GetBool("update")
		force, _ := cmd.Flags().GetBool("force")
		skill, _ := cmd.Flags().GetBool("skill")

		opts := install.MCPInstallOptions{
			PrintConfig: printCfg,
			Run:         run,
			Yes:         yes,
			Update:      update,
			Force:       force,
			Skill:       skill,
			Out:         os.Stdout,
			In:          os.Stdin,
		}

		if _, err := install.MCPInstall(ctx, opts); err != nil {
			return fmt.Errorf("guild mcp install: %w", err)
		}
		return nil
	},
}

func init() {
	mcpInstallCmd.Flags().Bool("print-config", false, "print JSON snippet for manual paste (no detection output)")
	mcpInstallCmd.Flags().Bool("run", false, "shell out to each detected client's CLI (prompts per command)")
	mcpInstallCmd.Flags().Bool("yes", false, "skip per-command confirmation prompts (combine with --run)")
	mcpInstallCmd.Flags().Bool("update", false, "re-register clients whose configured path differs from the running binary")
	mcpInstallCmd.Flags().Bool("force", false, "re-register every detected client unconditionally (implies --update)")
	mcpInstallCmd.Flags().Bool("skill", false, "install Claude Code skill (not yet implemented)")

	mcpCmd.AddCommand(mcpInstallCmd)
}
