package cli

import (
	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/command"
)

// eval.go owns the CLI parent for the eval capability module (ADR-006 Phase
// 6). The eval module is OFF by default, so unlike loreCmd/questCmd this
// parent is NOT unconditionally attached to rootCmd: bindModuleVerbs attaches
// it only when the module is enabled (see attachEvalParentIfEnabled). With a
// silent config the `eval` command never appears in `guild --help`, keeping
// the default CLI surface byte-identical to a build without the module.

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "deterministic recall/ranking evaluation (adversarial grid + golden parity)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

// buildCLIEvalDeps constructs the CLI-side Deps for the eval module. The
// eval_run handler is self-contained (its own in-memory corpus; never touches
// ~/.guild), so it needs no OpenDB / ResolveProj. An empty Deps suffices; the
// cobra adapter's telemetry wrap is applied by bindModuleVerb as for every
// other verb.
func buildCLIEvalDeps() command.Deps {
	return command.Deps{}
}
