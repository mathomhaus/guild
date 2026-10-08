package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/module"
	_ "github.com/mathomhaus/guild/internal/modules" // activate core modules (quest/lore/session) in the registry
)

// cliModuleEnabledPredicate loads the merged config and returns the
// predicate module.Enabled consults to decide which modules mount their
// CLI verbs (ADR-006 Phase 3). CLI verb binding happens at package init,
// before cobra parses flags, so this predicate reflects the file + env
// layers ([modules] table, GUILD_MODULE_<NAME> / GUILD_NO_<NAME>); the
// --module / --disable-module flags are still honored everywhere config is
// re-loaded with the parsed FlagSet (the MCP and daemon paths). On a
// config-load failure it returns nil, which module.Enabled treats as
// "every module on its own DefaultEnabled()" — a broken config must never
// silently strip the core verbs.
func cliModuleEnabledPredicate() func(name string, def bool) bool {
	cfg, err := config.Load(nil)
	if err != nil {
		return nil
	}
	return config.ModuleEnabled(cfg)
}

// bindModuleVerbs is the ADR-006 Phase 2 CLI cutover: it replaces the
// per-file hand lists of bindRegistryVerb / bindLoreRegistryVerb calls
// (quest.go, lore_read.go, lore_write.go, lore_health.go) with one loop over
// the module registry. For each enabled module it resolves that module's
// CLI-side Deps bundle (the loreDeps vs questDeps split is preserved, keyed
// by module name) and binds every Command the module contributes onto the
// matching cobra parent, then restores the per-verb telemetry wrap exactly
// as the old helpers did.
//
// It is called once from the cli package's init() (modules_init.go's init
// after questCmd/loreCmd exist). Command order is not observable: cobra sorts
// subcommands in help output and the daemon-route parity tests diff verb
// SETS, not order, so this loop is byte-identical to the old explicit lists.
func bindModuleVerbs() {
	for _, m := range module.Enabled(cliModuleEnabledPredicate()) {
		deps, parent, ok := cliBindTargetForModule(m.Name())
		if !ok {
			// session contributes no CLI verbs; its bootstrap tools are MCP
			// only and hand-wired. Skip rather than bind nil Commands.
			continue
		}
		// Opt-in module parents (off by default) are attached to rootCmd only
		// here, when the module is enabled, so a disabled module adds no
		// top-level command to `guild --help` and the default surface stays
		// byte-identical. Core parents (lore/quest) are already attached by
		// root.go's AddCommand and are skipped by attachModuleParentIfNeeded.
		attachModuleParentIfNeeded(m.Name(), parent)
		for _, cmd := range m.Commands() {
			bindModuleVerb(parent, cmd, deps)
		}
	}
}

// attachModuleParentIfNeeded attaches an opt-in module's cobra parent to
// rootCmd the first time the module is bound, idempotently. Core parents
// (lore, quest) are mounted unconditionally by root.go and must not be
// double-added, so they are excluded here. An opt-in parent (eval) is added
// only when its module is enabled, which is the mechanism that keeps the
// default `guild --help` surface byte-identical.
func attachModuleParentIfNeeded(name string, parent *cobra.Command) {
	switch name {
	case "eval":
		if parent != nil && parent.Parent() == nil {
			rootCmd.AddCommand(parent)
		}
	default:
		// lore/quest: already attached by root.go; nothing to do.
	}
}

// cliBindTargetForModule returns the CLI Deps bundle and cobra parent a
// module's verbs attach to, keyed by module name. ok=false means the module
// contributes no CLI verbs through the loop (session).
//
// A module not in the core set (e.g. an enabled compression module) gets a
// generated top-level parent command, created on demand and attached to
// rootCmd here. The loop only iterates ENABLED modules, so a disabled
// compression module never reaches this branch and never adds a `compression`
// command to the CLI — the default surface stays byte-identical.
func cliBindTargetForModule(name string) (command.Deps, *cobra.Command, bool) {
	switch name {
	case "lore":
		return buildCLILoreDeps(), loreCmd, true
	case "quest":
		return buildCLICommandDeps(), questCmd, true
	case "eval":
		return buildCLIEvalDeps(), evalCmd, true
	case "session":
		return command.Deps{}, nil, false
	default:
		return command.Deps{}, ensureModuleParent(name), true
	}
}

// moduleParents holds the generated top-level parent command for each
// non-core enabled module, created once and attached to rootCmd.
var moduleParents = map[string]*cobra.Command{}

// ensureModuleParent returns (creating + attaching once) the top-level cobra
// parent for a non-core module's verbs.
func ensureModuleParent(name string) *cobra.Command {
	if c, ok := moduleParents[name]; ok {
		return c
	}
	c := &cobra.Command{
		Use:   name,
		Short: name + " capability (opt-in module)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	moduleParents[name] = c
	rootCmd.AddCommand(c)
	return c
}

// cliBespokeVerbs are the registry tool names whose CLI surface is NOT bound
// by the module loop. They fall into two groups, both byte-identical to the
// pre-cutover wiring:
//
//   - lore_appraise / lore_study have hand-rolled cobra commands
//     (newAppraiseCmd / newStudyCmd) with CLI-only hook-mode flags (--inject,
//     --from-stdin-json, --query) that the registry spec does not carry, so
//     the loop must not also register a colliding `lore appraise` subcommand.
//     They remain registry tools on the MCP surface (bound by the MCP loop).
//   - quest_clear is MCPOnly: it was never passed to bindRegistryVerb on the
//     CLI before, so it never appeared in cliRegistryBoundVerbs. command.
//     BindCobra would skip it anyway, but listing it here keeps it out of the
//     CLI-bound tracking slice exactly as before.
var cliBespokeVerbs = map[string]bool{
	"lore_appraise": true,
	"lore_study":    true,
	"quest_clear":   true,
}

// bindModuleVerb attaches one registry spec to parent and restores the
// telemetry wrap + the cliRegistryBoundVerbs tracking append, the same three
// effects bindRegistryVerb / bindLoreRegistryVerb produced per verb. The
// per-verb telemetry label is strings.Join(CobraPath, " "), which reproduces
// every pre-cutover label exactly (e.g. quest_epic -> "quest campaign",
// lore_health -> "lore health").
func bindModuleVerb(parent *cobra.Command, spec command.Registrant, deps command.Deps) {
	if cliBespokeVerbs[spec.WireName()] {
		return
	}
	spec.BindCobra(parent, deps)
	path := spec.CobraPath()
	if len(path) == 0 {
		return
	}
	wrapTelemetry(parent, path[len(path)-1], strings.Join(path, " "))
	cliRegistryBoundVerbs = append(cliRegistryBoundVerbs, spec.WireName())
}
