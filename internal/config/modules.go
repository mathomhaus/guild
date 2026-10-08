package config

import (
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/spf13/pflag"
)

// This file is the ADR-006 Phase 3 config spine for capability modules:
// the [modules] toggle table, the per-module config-section registry that
// lets fileLayer stop hand-coding one IsDefined branch per key, the
// GUILD_MODULE_* / GUILD_NO_* env + --module/--no-<name> flag overrides,
// and the ModuleEnabled predicate every surface passes to module.Enabled.

// ModuleConfigMerger applies a module's own config section onto the merged
// Config. It receives the same per-file MetaData the core fileLayer uses,
// so a module can do granular per-key IsDefined merges (a key absent from
// THIS file must keep the value a lower layer set). path is the file being
// merged, for error messages. raw is the already-decoded top-level table
// for the file (a module reads its own subsection by name from md/raw).
//
// A merger must not panic on a missing section: a file that omits the
// module's [<name>] table simply leaves the lower layer's values intact.
type ModuleConfigMerger func(md toml.MetaData, raw map[string]any, path string, dst *Config) error

var (
	modCfgMu       sync.Mutex
	moduleConfigs  = map[string]ModuleConfigMerger{}
	moduleDefaults = map[string]func(*Config){}
)

// RegisterModuleConfig lets a capability-module package self-register its
// config section's merge and (optionally) its built-in defaults, so
// internal/config no longer grows a hand-coded branch per module key. Call
// from the module package's init(), exactly like module.Register:
//
//	func init() {
//	    config.RegisterModuleConfig("compression", mergeCompression, defaultsCompression)
//	}
//
// name is the module's [<name>] TOML section and its [modules] toggle key.
// merge runs inside fileLayer for every config file (user, then repo), in
// the same precedence order as the core knobs. applyDefaults, when
// non-nil, seeds the baseline before any file layer (so a partial file
// override keeps the module's defaults for absent keys); pass nil for a
// module whose zero values are already its defaults. Panics on an empty or
// duplicate name (a programmer error at init time).
func RegisterModuleConfig(name string, merge ModuleConfigMerger, applyDefaults func(*Config)) {
	if name == "" {
		panic("config: RegisterModuleConfig called with empty module name")
	}
	modCfgMu.Lock()
	defer modCfgMu.Unlock()
	if _, dup := moduleConfigs[name]; dup {
		panic("config: RegisterModuleConfig called twice for module " + name)
	}
	moduleConfigs[name] = merge
	if applyDefaults != nil {
		moduleDefaults[name] = applyDefaults
	}
}

// applyModuleDefaults runs every registered module's default seeder onto
// cfg, in name-sorted order for determinism. Called from defaults() so a
// module's built-in config values are present before any file layer.
func applyModuleDefaults(cfg *Config) {
	modCfgMu.Lock()
	names := make([]string, 0, len(moduleDefaults))
	for n := range moduleDefaults {
		names = append(names, n)
	}
	fns := make([]func(*Config), 0, len(names))
	sort.Strings(names)
	for _, n := range names {
		fns = append(fns, moduleDefaults[n])
	}
	modCfgMu.Unlock()
	for _, fn := range fns {
		fn(cfg)
	}
}

// applyModuleConfigLayers runs every registered module merger for one
// config file, in name-sorted order, so module sections merge with the
// same per-key granularity and determinism as the core knobs. Called from
// fileLayer after the core sections are applied.
func applyModuleConfigLayers(md toml.MetaData, raw map[string]any, path string, dst *Config) error {
	modCfgMu.Lock()
	names := make([]string, 0, len(moduleConfigs))
	for n := range moduleConfigs {
		names = append(names, n)
	}
	mergers := make([]ModuleConfigMerger, 0, len(names))
	sort.Strings(names)
	for _, n := range names {
		mergers = append(mergers, moduleConfigs[n])
	}
	modCfgMu.Unlock()
	for _, m := range mergers {
		if err := m(md, raw, path, dst); err != nil {
			return err
		}
	}
	return nil
}

// mergeModulesTable applies the [modules] toggle table from one decoded
// file onto dst per key, so a module toggle absent from THIS file keeps
// whatever a lower layer (preset, or the user file under the repo file)
// resolved. Only keys the file actually declares are copied.
func mergeModulesTable(md toml.MetaData, src ModulesConfig, dst *Config) {
	if len(src) == 0 {
		return
	}
	if dst.Modules == nil {
		dst.Modules = make(ModulesConfig, len(src))
	}
	for name, on := range src {
		if md.IsDefined("modules", name) {
			dst.Modules[name] = on
		}
	}
}

// envModuleOverrides applies GUILD_MODULE_<NAME> and GUILD_NO_<NAME> onto
// the [modules] table (ADR-006 Phase 3, env layer). It follows the
// established GUILD_NO_* convention: GUILD_NO_<NAME>=1 forces the module
// off; GUILD_MODULE_<NAME> sets it explicitly (truthy on, falsy off). The
// name segment is the module's Name() upper-cased (e.g. lore -> LORE);
// GUILD_NO_<NAME> wins over GUILD_MODULE_<NAME> when both are set, matching
// the "disable switch is final" posture of GUILD_NO_DAEMON et al.
//
// Discovery: the env layer cannot enumerate registered modules without an
// import cycle (internal/module would have to import internal/config and
// vice versa), so it scans the process environment for the GUILD_MODULE_ /
// GUILD_NO_ prefixes and records every toggle it finds. An override naming
// a module that turns out not to be registered is harmless: ModuleEnabled
// is only ever consulted for registered modules, so a stray key is never
// read.
func envModuleOverrides(dst *Config) {
	const (
		modPrefix = "GUILD_MODULE_"
		noPrefix  = "GUILD_NO_"
	)
	// Known GUILD_NO_* knobs that are NOT module toggles. Treating these
	// as module disables would invent phantom toggle keys ("usage_log",
	// "emoji", ...); the env layer already handles them by name above.
	reserved := map[string]bool{
		"USAGE_LOG":    true,
		"EMOJI":        true,
		"DAEMON":       true,
		"SLEEP":        true,
		"WATCH":        true,
		"UPDATE_CHECK": true,
	}
	set := func(name string, on bool) {
		if dst.Modules == nil {
			dst.Modules = ModulesConfig{}
		}
		dst.Modules[strings.ToLower(name)] = on
	}
	// GUILD_MODULE_<NAME> first; GUILD_NO_<NAME> applied after so a
	// disable switch wins when both are present.
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		name, ok := strings.CutPrefix(key, modPrefix)
		if !ok || name == "" {
			continue
		}
		set(name, truthy(val))
	}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		name, ok := strings.CutPrefix(key, noPrefix)
		if !ok || name == "" || reserved[name] {
			continue
		}
		if truthy(val) {
			set(name, false)
		}
	}
}

// truthy reports whether an env value reads as on. Mirrors parseBoolEnv's
// acceptance ("1"/"true"/"yes") for module toggles.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y", "t":
		return true
	default:
		return false
	}
}

// flagModuleOverrides applies the CLI flag layer for module toggles
// (ADR-006 Phase 3, flag layer, highest precedence). Two flags, both
// optional and silently skipped when the FlagSet does not define them:
//
//   - --module name=bool (repeatable): an explicit per-module toggle,
//     e.g. --module lore=false --module compression=true.
//   - --no-<module> (a StringSlice of names is overkill; we use a single
//     repeatable --disable-module name): forces a module off, mirroring
//     --no-daemon. Kept as --disable-module so it composes with arbitrary
//     module names without predefining one flag per module.
func flagModuleOverrides(flags *pflag.FlagSet, dst *Config) {
	if flags == nil {
		return
	}
	if f := flags.Lookup("module"); f != nil && f.Changed {
		if ss, ok := f.Value.(pflag.SliceValue); ok {
			for _, pair := range ss.GetSlice() {
				name, on, valid := parseModuleFlag(pair)
				if valid {
					if dst.Modules == nil {
						dst.Modules = ModulesConfig{}
					}
					dst.Modules[name] = on
				}
			}
		}
	}
	if f := flags.Lookup("disable-module"); f != nil && f.Changed {
		if ss, ok := f.Value.(pflag.SliceValue); ok {
			for _, name := range ss.GetSlice() {
				name = strings.ToLower(strings.TrimSpace(name))
				if name == "" {
					continue
				}
				if dst.Modules == nil {
					dst.Modules = ModulesConfig{}
				}
				dst.Modules[name] = false
			}
		}
	}
}

// parseModuleFlag parses one --module value of the form name=bool. A bare
// name (no '=') is treated as name=true. An unparseable bool makes the
// pair invalid (skipped) so a typo never silently flips a module.
func parseModuleFlag(pair string) (name string, on, valid bool) {
	pair = strings.TrimSpace(pair)
	if pair == "" {
		return "", false, false
	}
	if eq := strings.IndexByte(pair, '='); eq >= 0 {
		name = strings.ToLower(strings.TrimSpace(pair[:eq]))
		on = truthy(pair[eq+1:])
		// Reject an unrecognized bool literal rather than defaulting it.
		if !boolish(pair[eq+1:]) {
			return name, false, false
		}
		return name, on, name != ""
	}
	return strings.ToLower(pair), true, pair != ""
}

// boolish reports whether v is a recognized on/off literal.
func boolish(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y", "t",
		"0", "false", "no", "off", "n", "f":
		return true
	default:
		return false
	}
}

// ModuleEnabled returns the predicate every surface passes to
// module.Enabled: given a module's Name() and its DefaultEnabled(), it
// returns the operator's final verdict by consulting the merged [modules]
// table. A name absent from the table keeps the module's own default; a
// present key overrides it. The predicate closes over a snapshot of the
// config so a long-lived MCP server can be re-derived per connect without
// re-reading the file each call.
//
// Returning a func(name, def) matches module.Enabled's seam exactly and
// keeps internal/module decoupled from internal/config (module never
// imports config; the kernel wires the two at the edge).
func ModuleEnabled(cfg *Config) func(name string, def bool) bool {
	var table ModulesConfig
	if cfg != nil {
		table = cfg.Modules
	}
	return func(name string, def bool) bool {
		if table != nil {
			if on, ok := table[name]; ok {
				return on
			}
		}
		return def
	}
}
