package hooks

import (
	"fmt"

	"github.com/spf13/cobra"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
)

func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "install guild lifecycle hooks into detected harnesses",
		Long: `guild hooks install: first-time hook setup

Detects which harnesses are present on this machine, writes the shared
base config to ~/.guild/hooks-base.json when missing, and renders it
into each detected harness's settings file through that harness's
adapter. Foreign hooks and unrelated settings are preserved untouched;
re-running on an already-installed harness is a no-op.

Flags:
  --harness=NAME  only install for one adapter (see 'guild hooks list')
  --dry-run       report what would be written; write nothing`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			harness, _ := cmd.Flags().GetString("harness")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			if err := runInstall(liveDeps(cmd.OutOrStdout()), harness, dryRun); err != nil {
				return fmt.Errorf("guild hooks install: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().String("harness", "", "only install for the named harness adapter")
	cmd.Flags().Bool("dry-run", false, "report what would be written; write nothing")
	return cmd
}

func runInstall(d deps, harness string, dryRun bool) error {
	// Validate the --harness filter before anything else, including the
	// zero-adapters early return below: a misspelled name must error in
	// every build, not silently exit 0 in an adapter-less one.
	selected, err := selectAdapters(d.adapters, harness)
	if err != nil {
		return err
	}

	// Base config next: install owns creating ~/.guild/hooks-base.json.
	basePath, err := hookcfg.BasePath()
	if err != nil {
		return err
	}
	var base hookcfg.Config
	if dryRun {
		base, err = hookcfg.LoadBase()
		if err != nil {
			return err
		}
		fmt.Fprintf(d.out, "base config: %s (dry-run: not written)\n", basePath)
	} else {
		var created bool
		base, created, err = hookcfg.EnsureBase()
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintf(d.out, "base config: %s (created with defaults)\n", basePath)
		} else {
			fmt.Fprintf(d.out, "base config: %s (existing)\n", basePath)
		}
	}
	fmt.Fprintln(d.out)

	// Harness detection: same registry `guild mcp install` reports from.
	adapterNames := map[string]bool{}
	for _, a := range d.adapters {
		adapterNames[a.Name()] = true
	}
	fmt.Fprintln(d.out, "Detected harnesses:")
	for _, c := range d.clients {
		switch {
		case !c.Detected():
			fmt.Fprintf(d.out, "  ✗ %s  (not detected)\n", c.Name)
		case adapterNames[adapterNameForClient(c.Name)]:
			fmt.Fprintf(d.out, "  ✓ %s\n", c.Name)
		default:
			fmt.Fprintf(d.out, "  ✓ %s  (hook support not yet available)\n", c.Name)
		}
	}
	fmt.Fprintln(d.out)

	if len(d.adapters) == 0 {
		fmt.Fprintln(d.out, "No hook adapters are registered in this build; nothing to install yet.")
		return nil
	}

	for _, ad := range selected {
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
		case dryRun && st == statusInSync:
			fmt.Fprintf(d.out, "%s: in sync at %s (dry-run: nothing to do)\n", ad.Name(), path)
		case dryRun:
			fmt.Fprintf(d.out, "%s: would write %s (currently %s)\n", ad.Name(), path, st)
		case st == statusInSync:
			fmt.Fprintf(d.out, "%s: already installed at %s (no-op)\n", ad.Name(), path)
		default:
			if err := ad.Install(base); err != nil {
				return fmt.Errorf("install hooks for %s: %w", ad.Name(), err)
			}
			fmt.Fprintf(d.out, "%s: installed hooks -> %s\n", ad.Name(), path)
		}
	}
	return nil
}
