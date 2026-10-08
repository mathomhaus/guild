package hooks

import (
	"fmt"

	"github.com/spf13/cobra"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
)

// ANSI color codes for diff lines. Color is opt-out (--no-color) and
// applied per line, never spanning newlines.
const (
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiReset = "\x1b[0m"
)

func newDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "show what 'guild hooks sync' would change (no writes)",
		Long: `guild hooks diff: preview sync

Compares the guild-owned hooks in each detected harness's settings file
against the base config and prints the difference: '-' lines are
guild-owned hooks sync would remove or rewrite, '+' lines are hooks it
would add. Foreign hooks never appear; sync does not touch them.
Nothing is written.

Flags:
  --no-color  plain output without ANSI colors`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			noColor, _ := cmd.Flags().GetBool("no-color")
			if err := runDiff(liveDeps(cmd.OutOrStdout()), noColor); err != nil {
				return fmt.Errorf("guild hooks diff: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().Bool("no-color", false, "plain output without ANSI colors")
	return cmd
}

func runDiff(d deps, noColor bool) error {
	base, err := hookcfg.LoadBase()
	if err != nil {
		return err
	}
	if len(d.adapters) == 0 {
		fmt.Fprintln(d.out, "No hook adapters are registered in this build; nothing to diff yet.")
		return nil
	}
	minus, plus := "- ", "+ "
	if !noColor {
		minus, plus = ansiRed+"- ", ansiGreen+"+ "
	}
	lineEnd := ""
	if !noColor {
		lineEnd = ansiReset
	}

	for _, ad := range d.adapters {
		detected, err := ad.Detect()
		if err != nil {
			return fmt.Errorf("detect %s: %w", ad.Name(), err)
		}
		if !detected {
			continue
		}
		st, path, err := targetState(ad, base)
		if err != nil {
			return err
		}
		fmt.Fprintf(d.out, "%s (%s): %s\n", ad.Name(), path, st)
		if st == statusInSync {
			continue
		}
		scanned, err := ad.Scan()
		if err != nil {
			return fmt.Errorf("scan %s: %w", ad.Name(), err)
		}
		current := guildOwnedOnly(scanned)
		desired, err := desiredHooks(ad, base)
		if err != nil {
			return err
		}

		inDesired := map[string]bool{}
		for _, h := range desired {
			inDesired[hookKey(h)] = true
		}
		inCurrent := map[string]bool{}
		for _, h := range current {
			inCurrent[hookKey(h)] = true
		}
		for _, h := range current {
			if !inDesired[hookKey(h)] {
				fmt.Fprintf(d.out, "  %s%s%s\n", minus, formatHook(h), lineEnd)
			}
		}
		for _, h := range desired {
			if !inCurrent[hookKey(h)] {
				fmt.Fprintf(d.out, "  %s%s%s\n", plus, formatHook(h), lineEnd)
			}
		}
	}
	return nil
}
