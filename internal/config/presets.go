package config

import (
	"fmt"
	"sort"
	"sync"
)

// Presets are named bundles of module toggles (ADR-006 Phase 3, [profile]
// preset). A preset expands into a Modules baseline applied BEFORE the
// file, env, and flag layers, so an explicit [modules] key (or a
// GUILD_MODULE_* override, or a --module flag) always wins over the preset
// it sits inside. This keeps "pick a profile, then tweak one module" a
// one-line change instead of restating the whole toggle set.
//
// The registry is seeded with three built-in presets (minimal, developer,
// full) and is open for a module package to extend via RegisterPreset, the
// same self-registration posture as RegisterModuleConfig.

var (
	presetMu sync.Mutex
	presets  = map[string]ModulesConfig{
		// minimal: only the three core pillars on, everything else off.
		// The portable "tasks + memory + context, nothing heavy" profile.
		"minimal": {
			"quest":   true,
			"lore":    true,
			"session": true,
		},
		// developer: the core pillars plus the developer-facing
		// capabilities a contributor wants on by default. Today that set
		// equals the core (no heavy modules have landed yet); it is listed
		// explicitly so the bundle is self-documenting and a future
		// observability/eval module flips on here without touching callers.
		"developer": {
			"quest":   true,
			"lore":    true,
			"session": true,
		},
		// full: every known module on. Built dynamically at expansion time
		// from the registered preset extensions is overkill; instead full
		// turns the core on and any module a RegisterPreset extension
		// contributed (see registerPresetModule) on too.
		"full": {
			"quest":   true,
			"lore":    true,
			"session": true,
		},
	}
)

// RegisterPreset adds or replaces a named preset's toggle set. Intended
// for a capability-module package that wants to participate in the
// built-in presets (e.g. a compression module adding itself to "full").
// Call from init(). A nil or empty toggles map is a no-op. Replacing a
// built-in preset is allowed (last writer wins) so a module can opt itself
// into "developer"/"full" by merging rather than redefining.
func RegisterPreset(name string, toggles ModulesConfig) {
	if name == "" || len(toggles) == 0 {
		return
	}
	presetMu.Lock()
	defer presetMu.Unlock()
	existing := presets[name]
	if existing == nil {
		existing = ModulesConfig{}
		presets[name] = existing
	}
	for k, v := range toggles {
		existing[k] = v
	}
}

// PresetNames returns the registered preset names, sorted. Used by error
// messages and tests.
func PresetNames() []string {
	presetMu.Lock()
	defer presetMu.Unlock()
	names := make([]string, 0, len(presets))
	for n := range presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// applyPreset expands the named preset into cfg.Modules as a BASELINE: it
// only sets a toggle the [modules] table has not already declared, so an
// explicit key always wins over the preset. An empty preset name is a
// no-op; an unknown name is an error (a typo must be loud). Called from
// Load after the file layers (so [modules] keys are already present) but
// the baseline-only merge means file keys still override the preset.
func applyPreset(name string, cfg *Config) error {
	if name == "" {
		return nil
	}
	presetMu.Lock()
	toggles, ok := presets[name]
	presetMu.Unlock()
	if !ok {
		return fmt.Errorf("config: [profile] preset %q is unknown (valid: %v)", name, PresetNames())
	}
	if cfg.Modules == nil {
		cfg.Modules = ModulesConfig{}
	}
	for module, on := range toggles {
		if _, explicit := cfg.Modules[module]; !explicit {
			cfg.Modules[module] = on
		}
	}
	return nil
}
