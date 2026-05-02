// Package main is the entry point for the guild binary.
//
// guild bundles the lore CLI, quest CLI, and MCP stdio server in one
// static binary. See https://github.com/mathomhaus/guild for docs.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mathomhaus/guild/internal/cli"
	"github.com/mathomhaus/guild/internal/release"
)

// version, commit, and date are stamped at build time via -ldflags.
// goreleaser sets them to the release tag, git SHA, and build date
// respectively. The defaults ("dev", "", "") apply to `go build` and
// `go install` invocations that don't pass ldflags.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	// Wire the build-time stamp values into the CLI before executing.
	cli.SetVersion(version, commit, date)

	// Wire the upgrade nudge. The PersistentPreRun in internal/cli/root.go
	// guards on stderr being a TTY before calling this function, so we pass
	// isTTY=true here (the TTY gate is already applied by the caller).
	// Background context: the nudge is a best-effort check; we do not want
	// the user's command to block on a deadline beyond the 2s already applied
	// inside CheckAndNudge.
	cli.SetUpgradeNudge(func() string {
		return release.CheckAndNudge(context.Background(), version, true)
	})

	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
