package compression

import (
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/mathomhaus/guild/internal/config"
)

// Module config for the [compression] TOML section, self-registered via the
// Phase-3 RegisterModuleConfig seam so internal/config never grows a
// compression-shaped field or branch. Because the module is OFF by default,
// this section is only consulted when [modules].compression = true; with the
// default config the section is unread and the snapshot stays at its zero
// (off-equivalent) value.
//
// internal/config.Config has no compression field (we are forbidden to edit
// the core struct), so the registered merger captures the parsed section into
// a package-local snapshot here, with per-key IsDefined granularity matching
// the core merge. fileLayer runs the merger once per config file in
// precedence order (user file, then repo file), so a key the repo file
// declares wins, and a key only the user file declares survives — exactly the
// core knobs' behavior.

// Settings is the resolved [compression] configuration.
type Settings struct {
	// Strategies names the compressor strategies that are live, in the
	// order they should be tried by the auto-detect path. Empty means "all
	// registered strategies, default order".
	Strategies []string

	// CCRTTL is the lifetime of CCR-stashed originals. Zero falls back to
	// DefaultTTL.
	CCRTTL time.Duration

	// DossierCompact enables the optional lore_dossier compaction path when
	// the module is enabled. Off by default so even with the module on the
	// dossier stays its normal shape unless explicitly opted in.
	DossierCompact bool
}

var (
	settingsMu sync.RWMutex
	settings   Settings
)

func init() {
	config.RegisterModuleConfig("compression", mergeCompression, nil)
}

// mergeCompression merges one config file's [compression] section into the
// package snapshot with per-key granularity. It writes into the package
// snapshot rather than *config.Config because the core struct carries no
// compression field by design.
func mergeCompression(md toml.MetaData, raw map[string]any, _ string, _ *config.Config) error {
	if _, ok := raw["compression"]; !ok {
		return nil // file omits the section: leave lower layers intact
	}
	// The [compression] table is already decoded into the raw map; read each
	// key via the MetaData IsDefined probe so a key this file omits keeps
	// whatever a lower layer set.
	sub, _ := raw["compression"].(map[string]any)

	settingsMu.Lock()
	defer settingsMu.Unlock()
	if md.IsDefined("compression", "strategies") {
		if v, ok := sub["strategies"].([]any); ok {
			out := make([]string, 0, len(v))
			for _, s := range v {
				if str, ok := s.(string); ok {
					out = append(out, str)
				}
			}
			settings.Strategies = out
		}
	}
	if md.IsDefined("compression", "ccr_ttl") {
		if s, ok := sub["ccr_ttl"].(string); ok {
			if d, err := time.ParseDuration(s); err == nil {
				settings.CCRTTL = d
			}
		}
	}
	if md.IsDefined("compression", "dossier_compact") {
		if b, ok := sub["dossier_compact"].(bool); ok {
			settings.DossierCompact = b
		}
	}
	return nil
}

// CurrentSettings returns a copy of the resolved [compression] settings.
func CurrentSettings() Settings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	s := settings
	if s.Strategies != nil {
		s.Strategies = append([]string(nil), s.Strategies...)
	}
	return s
}

// resetSettingsForTest restores the zero snapshot; used only by tests so one
// test's config does not leak into another.
func resetSettingsForTest() {
	settingsMu.Lock()
	settings = Settings{}
	settingsMu.Unlock()
}
