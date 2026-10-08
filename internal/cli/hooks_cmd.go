package cli

import (
	hookscmd "github.com/mathomhaus/guild/internal/cli/hooks"
)

// The `guild hooks` family lives in its own package
// (internal/cli/hooks) because it carries five subcommands plus shared
// drift/diff plumbing; only the root wiring belongs here.
func init() {
	rootCmd.AddCommand(hookscmd.New())
}
