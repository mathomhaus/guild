// Package hooks implements the `guild hooks` command family:
// install / sync / diff / list / scan for harness lifecycle hooks.
//
// The abstract base config lives at ~/.guild/hooks-base.json (see
// internal/hooks); per-harness rendering goes through the adapter
// registry (internal/hooks/adapters). Harness detection reuses the
// internal/install client registry, the same source of truth `guild
// mcp install` prints from.
package hooks

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
	"github.com/mathomhaus/guild/internal/install"
)

// New builds the `guild hooks` command tree. Called once from
// internal/cli during root assembly.
func New() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "manage harness lifecycle hooks (auto-inject guild context)",
		Long: `guild hooks: install and maintain harness lifecycle hooks

Lifecycle hooks make guild proactive: the harness runs guild commands
at session start (prime the brief), before compaction (capture a brief)
and on each prompt (inject relevant lore) without the agent having to
remember a tool call.

The shared source of truth is ~/.guild/hooks-base.json (created on
first install; edit it to override the defaults, then run
'guild hooks sync'). Per-harness settings files are derived from it by
adapters and never hand-edited by guild beyond the guild-owned hook
groups: a hook group is guild-owned only when every command in it
starts with 'guild'. Everything else in your settings files is
preserved untouched.

The base config holds guild commands only; a command that does not
start with 'guild' is rejected when the file is read. Custom hooks
belong directly in the harness settings file, where sync preserves
them untouched.

Status vocabulary used by list/diff:
  in-sync   guild-owned hooks match the base config
  drift     guild-owned hooks present but differ from the base config
  missing   no guild-owned hooks (or no settings file) for the harness`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newInstallCmd(), newSyncCmd(), newDiffCmd(), newListCmd(), newScanCmd())
	return cmd
}

// deps bundles the injectable inputs of every hooks verb so tests can
// run against fake adapters and clients without the global registry.
type deps struct {
	adapters []adapters.Adapter
	clients  []install.Client
	out      io.Writer
}

// liveDeps assembles the production wiring: the global adapter
// registry plus the supported-client registry from internal/install.
func liveDeps(out io.Writer) deps {
	return deps{adapters: adapters.All(), clients: install.Clients, out: out}
}

// syncStatus is the three-way state of one harness target.
type syncStatus string

const (
	statusInSync  syncStatus = "in-sync"
	statusDrift   syncStatus = "drift"
	statusMissing syncStatus = "missing"
)

// targetState computes one adapter's sync status against the base
// config: missing when the settings file is absent or holds no
// guild-owned hooks, drift when guild-owned hooks differ from the
// rendered base, in-sync otherwise.
func targetState(ad adapters.Adapter, base hookcfg.Config) (st syncStatus, path string, err error) {
	path, err = ad.SettingsPath()
	if err != nil {
		return "", "", fmt.Errorf("hooks: settings path for %s: %w", ad.Name(), err)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return statusMissing, path, nil
		}
		return "", path, fmt.Errorf("hooks: stat %s: %w", path, err)
	}
	scanned, err := ad.Scan()
	if err != nil {
		return "", path, fmt.Errorf("hooks: scan %s: %w", ad.Name(), err)
	}
	current := guildOwnedOnly(scanned)
	if len(current) == 0 {
		return statusMissing, path, nil
	}
	desired, err := desiredHooks(ad, base)
	if err != nil {
		return "", path, err
	}
	if hooksEqual(current, desired) {
		return statusInSync, path, nil
	}
	return statusDrift, path, nil
}

// desiredHooks renders the base config through the adapter's
// Substitute (and, for adapters that implement adapters.Renderer, the
// adapter's harness-capability Render) and flattens it for comparison
// against Scan output. Consulting Render here keeps drift status in
// agreement with what Install/Sync actually write for harnesses that
// cannot represent the base config one-to-one.
// Ownership is keyed on the raw command string, so adapters whose
// Substitute rewrites the leading "guild" token must keep their own
// ownership bookkeeping; the stub and the JSON-shaped adapters do not
// rewrite it.
func desiredHooks(ad adapters.Adapter, base hookcfg.Config) ([]hookcfg.Hook, error) {
	cfg := hookcfg.ApplySubstitution(base, ad.Substitute)
	if r, ok := ad.(adapters.Renderer); ok {
		rendered, err := r.Render(cfg)
		if err != nil {
			return nil, fmt.Errorf("hooks: render desired state for %s: %w", ad.Name(), err)
		}
		cfg = rendered
	}
	return sortedHooks(hookcfg.Flatten(cfg)), nil
}

// guildOwnedOnly filters the flattened scan view down to guild-owned
// hooks, sorted for deterministic comparison.
func guildOwnedOnly(hs []hookcfg.Hook) []hookcfg.Hook {
	var out []hookcfg.Hook
	for _, h := range hs {
		if h.GuildOwned {
			out = append(out, h)
		}
	}
	return sortedHooks(out)
}

// sortedHooks returns hs sorted by (event, matcher, command). The input
// slice is sorted in place and returned for chaining.
func sortedHooks(hs []hookcfg.Hook) []hookcfg.Hook {
	sort.Slice(hs, func(i, j int) bool { return hookKey(hs[i]) < hookKey(hs[j]) })
	return hs
}

// hookKey is the comparison identity of one flattened hook.
func hookKey(h hookcfg.Hook) string {
	return h.Event + "\x00" + h.Matcher + "\x00" + h.Command
}

// hooksEqual reports whether two sorted flattened hook lists carry the
// same (event, matcher, command) tuples.
func hooksEqual(a, b []hookcfg.Hook) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if hookKey(a[i]) != hookKey(b[i]) {
			return false
		}
	}
	return true
}

// formatHook renders one flattened hook for human output.
func formatHook(h hookcfg.Hook) string {
	if h.Matcher == "" {
		return fmt.Sprintf("%s :: %s", h.Event, h.Command)
	}
	return fmt.Sprintf("%s [%s] :: %s", h.Event, h.Matcher, h.Command)
}

// selectAdapters applies the --harness filter. An empty filter keeps
// every adapter; an unknown name is an error listing what exists.
func selectAdapters(all []adapters.Adapter, harness string) ([]adapters.Adapter, error) {
	if harness == "" {
		return all, nil
	}
	for _, a := range all {
		if a.Name() == harness {
			return []adapters.Adapter{a}, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, a := range all {
		names = append(names, a.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("unknown harness %q: no hook adapters are registered in this build", harness)
	}
	return nil, fmt.Errorf("unknown harness %q: registered adapters: %s", harness, strings.Join(names, ", "))
}

// adapterNameForClient normalizes an install.Client display name into
// the adapter registry key: lowercase, parenthetical dropped, spaces to
// dashes. "Claude Code" -> "claude-code", "Codex (OpenAI)" -> "codex".
func adapterNameForClient(name string) string {
	if i := strings.Index(name, "("); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(strings.ToLower(name))
	return strings.ReplaceAll(name, " ", "-")
}
