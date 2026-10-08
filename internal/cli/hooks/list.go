package hooks

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
)

// listRow is one managed target in `guild hooks list` output.
type listRow struct {
	Harness  string `json:"harness"`
	Detected bool   `json:"detected"`
	Status   string `json:"status"`
	Path     string `json:"path"`
}

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "show all managed hook targets with sync status",
		Long: `guild hooks list: managed targets and their state

One row per registered adapter: whether the harness is detected on
this machine, the settings file the adapter manages, and the sync
status (in-sync / drift / missing).

Flags:
  --json  machine-readable output`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			if err := runList(liveDeps(cmd.OutOrStdout()), asJSON); err != nil {
				return fmt.Errorf("guild hooks list: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "machine-readable output")
	return cmd
}

func runList(d deps, asJSON bool) error {
	base, err := hookcfg.LoadBase()
	if err != nil {
		return err
	}
	rows := make([]listRow, 0, len(d.adapters))
	for _, ad := range d.adapters {
		detected, err := ad.Detect()
		if err != nil {
			return fmt.Errorf("detect %s: %w", ad.Name(), err)
		}
		st, path, err := targetState(ad, base)
		if err != nil {
			return err
		}
		rows = append(rows, listRow{
			Harness:  ad.Name(),
			Detected: detected,
			Status:   string(st),
			Path:     path,
		})
	}

	if asJSON {
		enc := json.NewEncoder(d.out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Fprintln(d.out, "No hook adapters are registered in this build.")
		return nil
	}
	fmt.Fprintf(d.out, "%-16s %-10s %-8s %s\n", "HARNESS", "DETECTED", "STATUS", "SETTINGS")
	for _, r := range rows {
		detected := "no"
		if r.Detected {
			detected = "yes"
		}
		fmt.Fprintf(d.out, "%-16s %-10s %-8s %s\n", r.Harness, detected, r.Status, r.Path)
	}
	return nil
}
