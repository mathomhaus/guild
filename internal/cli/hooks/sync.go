package hooks

import (
	"fmt"

	"github.com/spf13/cobra"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "regenerate per-harness hook settings from the base config",
		Long: `guild hooks sync: propagate ~/.guild/hooks-base.json

Re-renders the base config into every detected harness's settings file.
Guild-owned hook groups are replaced in place and missing ones
appended; foreign and mixed groups are preserved untouched. Idempotent:
a second run with nothing changed writes nothing.

Flags:
  --dry-run  report what would change; write nothing`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			if err := runSync(liveDeps(cmd.OutOrStdout()), dryRun); err != nil {
				return fmt.Errorf("guild hooks sync: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().Bool("dry-run", false, "report what would change; write nothing")
	return cmd
}

func runSync(d deps, dryRun bool) error {
	base, err := hookcfg.LoadBase()
	if err != nil {
		return err
	}
	if len(d.adapters) == 0 {
		fmt.Fprintln(d.out, "No hook adapters are registered in this build; nothing to sync yet.")
		return nil
	}
	for _, ad := range d.adapters {
		detected, err := ad.Detect()
		if err != nil {
			return fmt.Errorf("detect %s: %w", ad.Name(), err)
		}
		if !detected {
			fmt.Fprintf(d.out, "%s: skipped (harness not detected)\n", ad.Name())
			continue
		}
		st, path, err := targetState(ad, base)
		if err != nil {
			return err
		}
		switch {
		case st == statusInSync:
			fmt.Fprintf(d.out, "%s: in sync (no-op)\n", ad.Name())
		case dryRun:
			fmt.Fprintf(d.out, "%s: would update %s (currently %s)\n", ad.Name(), path, st)
		default:
			if err := ad.Sync(base); err != nil {
				return fmt.Errorf("sync hooks for %s: %w", ad.Name(), err)
			}
			fmt.Fprintf(d.out, "%s: synced -> %s\n", ad.Name(), path)
		}
	}
	return nil
}
