package observability

import (
	"errors"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/mathomhaus/guild/internal/config"
)

// Config-validation sentinels for the [observability] section. They surface
// through config.Load so a typo (wrong type for a known key) fails loud.
var (
	errBadSection     = errors.New("config: [observability] must be a table")
	errBadMetricsAddr = errors.New("config: [observability].metrics_addr must be a string")
	errBadEventLog    = errors.New("config: [observability].event_log must be a boolean")
)

// This file is the observability module's config surface, registered onto
// the ADR-006 Phase 3 RegisterModuleConfig seam in init() (module.go). The
// kernel's internal/config does NOT carry an [observability] field on its
// core Config struct (the ADR keeps core config edits out of new-capability
// modules), so the module owns its own config state here: the merger reads
// the file's [observability] subsection from the raw decoded table and folds
// it into a package-global Settings, and the daemon Service reads that
// Settings when it starts.
//
// The package-global is safe because config.Load runs to completion before
// the daemon collects module Services (cmd/guild/daemon.go: loadDaemonConfig
// then enabledModuleServices), so the Settings the Service reads reflect the
// fully-merged config. A mutex guards concurrent Loads (tests load config
// repeatedly) so the global is never read mid-write.

// Settings is the resolved [observability] config section.
type Settings struct {
	// MetricsAddr is the listen address for the Prometheus /metrics HTTP
	// endpoint, e.g. "127.0.0.1:9090". Empty disables the metrics endpoint (the
	// Service still records into the event log if EventLog is on). The
	// module being disabled is the primary off switch; this is the
	// secondary "module on but I don't want the HTTP port" knob.
	MetricsAddr string `toml:"metrics_addr"`

	// EventLog enables the durable append-only JSONL event log under the
	// guild home. On by default when the module is enabled: the event log
	// is the cheap, always-useful half of the observability triad.
	EventLog bool `toml:"event_log"`
}

// defaultSettings is the module's built-in baseline, applied before any file
// layer so a partial [observability] section keeps these defaults for absent
// keys. The default MetricsAddr is the conventional Prometheus port; EventLog
// defaults on. Note: none of this takes effect unless the module is enabled
// (DefaultEnabled is false), so a silent config contributes nothing.
func defaultSettings() Settings {
	return Settings{
		MetricsAddr: "127.0.0.1:9090",
		EventLog:    true,
	}
}

var (
	settingsMu sync.Mutex
	settings   = defaultSettings()
)

// CurrentSettings returns a copy of the resolved observability settings.
// Read by the Service at Start. Safe for concurrent use.
func CurrentSettings() Settings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return settings
}

// applyDefaults seeds the module's baseline into the package-global before
// any config file layer. It takes *config.Config to satisfy the
// RegisterModuleConfig signature, but the observability config lives in this
// package's global rather than on the core Config (the ADR keeps the core
// struct free of new-module fields), so it resets the global to the baseline
// and ignores the passed Config.
func applyDefaults(_ *config.Config) {
	settingsMu.Lock()
	settings = defaultSettings()
	settingsMu.Unlock()
}

// rawSection is the typed shape of an [observability] subsection, decoded
// from the file's raw top-level table.
type rawSection struct {
	MetricsAddr *string `toml:"metrics_addr"`
	EventLog    *bool   `toml:"event_log"`
}

// mergeConfig is the ModuleConfigMerger for [observability]. It reads the
// module's own [observability] subsection from the raw decoded file table
// with per-key granularity: a key absent from THIS file leaves the value a
// lower layer set, exactly like the core fileLayer's IsDefined merges. A file
// that omits the [observability] table leaves the current settings untouched.
//
// It folds into the package-global rather than onto dst (*config.Config), for
// the reason documented at applyDefaults. The merger uses pointer fields on
// rawSection so a present-but-empty value (e.g. metrics_addr = "") is honored
// as an explicit override rather than confused with "absent".
func mergeConfig(md toml.MetaData, raw map[string]any, _ string, _ *config.Config) error {
	if !md.IsDefined("observability") {
		return nil
	}
	sub, ok := raw["observability"]
	if !ok {
		return nil
	}
	// Re-decode the subsection into typed pointer fields. We round-trip the
	// already-decoded map through the TOML encoder/decoder pair so per-key
	// presence (pointer non-nil) is preserved without re-reading the file.
	var sec rawSection
	if err := remapSection(sub, &sec); err != nil {
		return err
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if sec.MetricsAddr != nil {
		settings.MetricsAddr = *sec.MetricsAddr
	}
	if sec.EventLog != nil {
		settings.EventLog = *sec.EventLog
	}
	return nil
}

// remapSection coerces the generic decoded subsection (a map[string]any from
// BurntSushi/toml) into the typed rawSection. It reads the two known keys
// directly with type assertions rather than re-encoding, so a malformed value
// (wrong type) is a load error naming nothing exotic, and an absent key leaves
// its pointer nil (per-key granularity).
func remapSection(sub any, dst *rawSection) error {
	m, ok := sub.(map[string]any)
	if !ok {
		// An [observability] declared as a non-table is a config error.
		return errBadSection
	}
	if v, ok := m["metrics_addr"]; ok {
		s, ok := v.(string)
		if !ok {
			return errBadMetricsAddr
		}
		dst.MetricsAddr = &s
	}
	if v, ok := m["event_log"]; ok {
		b, ok := v.(bool)
		if !ok {
			return errBadEventLog
		}
		dst.EventLog = &b
	}
	return nil
}
