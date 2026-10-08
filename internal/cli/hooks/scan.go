package hooks

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
)

// validator is the optional interface an adapter implements to report
// matcher values its harness can never dispatch on. Scan type-asserts
// every registered adapter against it, so the wiring stays
// registry-driven: any adapter that grows a Validate method lights up
// here without scan learning a harness name. The signature mirrors the
// adapters' exported dead-matcher check: warnings are written-through
// but never-firing matchers, err is a hard contract violation.
type validator interface {
	Validate(cfg hookcfg.Config) (warnings []string, err error)
}

// scanReport is one harness's scan result in `guild hooks scan` output.
type scanReport struct {
	Harness  string         `json:"harness"`
	Path     string         `json:"path"`
	Hooks    []hookcfg.Hook `json:"hooks"`
	Warnings []string       `json:"warnings,omitempty"`
}

func newScanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "report the hooks currently present in each harness's settings",
		Long: `guild hooks scan: read-only inventory

Reads each registered adapter's settings file and reports every hook
found there, guild-owned and foreign alike. Foreign hooks are tagged so
you can see what else manages this harness. Adapters that validate their
harness's matcher vocabulary also flag dead matchers: values that parse
but match none of the documented dispatch values, so the hook group can
never fire. Nothing is written.

Flags:
  --verbose  also report harnesses with no hooks installed
  --json     machine-readable output`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			verbose, _ := cmd.Flags().GetBool("verbose")
			asJSON, _ := cmd.Flags().GetBool("json")
			if err := runScan(liveDeps(cmd.OutOrStdout()), verbose, asJSON); err != nil {
				return fmt.Errorf("guild hooks scan: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "machine-readable output")
	return cmd
}

func runScan(d deps, verbose, asJSON bool) error {
	if len(d.adapters) == 0 && !asJSON {
		fmt.Fprintln(d.out, "No hook adapters are registered in this build.")
		return nil
	}
	reports := make([]scanReport, 0, len(d.adapters))
	for _, ad := range d.adapters {
		path, err := ad.SettingsPath()
		if err != nil {
			return fmt.Errorf("settings path for %s: %w", ad.Name(), err)
		}
		hs, err := ad.Scan()
		if err != nil {
			return fmt.Errorf("scan %s: %w", ad.Name(), err)
		}
		if len(hs) == 0 && !verbose && !asJSON {
			continue
		}
		reports = append(reports, scanReport{
			Harness:  ad.Name(),
			Path:     path,
			Hooks:    hs,
			Warnings: validateScanned(ad, hs),
		})
	}

	if asJSON {
		enc := json.NewEncoder(d.out)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	}

	if len(reports) == 0 {
		fmt.Fprintln(d.out, "No hooks found in any harness settings file.")
		return nil
	}
	for _, r := range reports {
		fmt.Fprintf(d.out, "%s (%s):\n", r.Harness, r.Path)
		if len(r.Hooks) == 0 {
			fmt.Fprintln(d.out, "  (no hooks)")
		}
		for _, h := range r.Hooks {
			tag := "[foreign]"
			if h.GuildOwned {
				tag = "[guild]  "
			}
			fmt.Fprintf(d.out, "  %s %s\n", tag, formatHook(h))
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(d.out, "  ! %s\n", w)
		}
	}
	return nil
}

// validateScanned runs the adapter's matcher-vocabulary check over the
// hooks actually present in its settings file, returning one warning
// line per finding. Adapters that do not implement validator (the
// harness has no closed matcher vocabulary, or has not wired a check)
// contribute nothing. A hard contract violation in the existing file is
// surfaced as a warning rather than failing the scan: scan is a
// read-only inventory and must report a broken settings file, not abort
// on it.
func validateScanned(ad adapters.Adapter, hs []hookcfg.Hook) []string {
	v, ok := ad.(validator)
	if !ok {
		return nil
	}
	warnings, err := v.Validate(configFromHooks(hs))
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	return warnings
}

// configFromHooks rebuilds the abstract event/matcher config from the
// flattened scan view so an adapter's Validate (which reasons over
// events and matchers) can run against what is actually installed. The
// command list inside each group is irrelevant to matcher validation,
// so groups are keyed on (event, matcher) and carry no commands. Output
// is deterministic: events and matchers are emitted in sorted order.
func configFromHooks(hs []hookcfg.Hook) hookcfg.Config {
	type key struct{ event, matcher string }
	seen := make(map[key]bool, len(hs))
	cfg := make(hookcfg.Config)
	for _, h := range hs {
		k := key{h.Event, h.Matcher}
		if seen[k] {
			continue
		}
		seen[k] = true
		cfg[h.Event] = append(cfg[h.Event], hookcfg.Group{Matcher: h.Matcher})
	}
	for ev := range cfg {
		groups := cfg[ev]
		sort.Slice(groups, func(i, j int) bool { return groups[i].Matcher < groups[j].Matcher })
	}
	return cfg
}
