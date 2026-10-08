package eval

import (
	"fmt"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/mathomhaus/guild/internal/config"
)

// config.go wires the eval module's [eval] config section through the
// ADR-006 Phase-3 RegisterModuleConfig seam. The merge runs inside the
// kernel's per-file config layer, in the same precedence order as the core
// knobs, WITHOUT internal/config growing a hand-coded branch for eval and
// WITHOUT a field on the core Config struct.
//
// The seam was designed for modules that own a field on *Config; eval owns
// none, so it side-channels the parsed values into a store KEYED BY THE
// DESTINATION *config.Config POINTER. Keying by pointer is what makes the
// side-channel correct under config.Load's layering: applyEvalDefaults seeds
// the entry for the Config Load is building, mergeEval updates that same
// entry per file with per-key granularity, and any unrelated defaults() call
// Load makes internally (e.g. its trailing daemon-fallback baseline) lands on
// a DIFFERENT pointer's entry and cannot clobber the real one. ConfigFor then
// resolves the values for a loaded *Config by that pointer.
//
// The section is pure-data policy for the eval_run verb and carries no
// behavior that could alter another module's surface; combined with
// DefaultEnabled()==false, a silent config leaves the default CLI / MCP /
// INSTRUCTIONS surface byte-identical.

// EvalConfig is the [eval] section. Both knobs are deterministic, no-LLM
// policy: they shape how the `eval run` verb reports, never what the grid
// computes.
type EvalConfig struct {
	// Strict makes `eval run` return a non-nil error (CLI non-zero exit /
	// MCP error result) when the grid misses its green floor or the parity
	// harness drifts. Default false: the verb reports the verdicts and exits
	// clean, so an exploratory run never fails a pipeline. A CI gate sets
	// [eval].strict = true (or passes --strict) to make drift fatal.
	Strict bool
	// MinGreen is the minimum number of GREEN grid cells required before the
	// run is considered acceptable under Strict. Zero (the default) means
	// "all cells must be green". A positive value lets an operator tolerate a
	// known-red cell while still gating on the rest.
	MinGreen int
}

// defaultEvalConfig is the built-in baseline, applied before any file layer.
func defaultEvalConfig() EvalConfig {
	return EvalConfig{Strict: false, MinGreen: 0}
}

var (
	cfgMu sync.Mutex
	// cfgByConfig maps a *config.Config (the one Load is assembling) to that
	// load's resolved [eval] values. Bounded by maxTrackedConfigs so a
	// long-lived process that re-loads config many times never leaks.
	cfgByConfig = map[*config.Config]*EvalConfig{}
	// cfgOrder tracks insertion order for FIFO eviction.
	cfgOrder []*config.Config
	// testOverride, when non-nil, short-circuits ConfigFor so tests can drive
	// the strict gate without a real config.Load.
	testOverride *EvalConfig
)

// maxTrackedConfigs caps the side-channel map. Config loads are infrequent
// (CLI: one per process; MCP: one per server construct), so a small cap is
// ample and bounds memory without coordination.
const maxTrackedConfigs = 64

// registerConfig self-registers the [eval] merger and defaults with the
// kernel config layer. Called from the module package init() alongside
// module.Register, exactly as ADR-006 prescribes.
func registerConfig() {
	config.RegisterModuleConfig("eval", mergeEval, applyEvalDefaults)
}

// applyEvalDefaults seeds the [eval] baseline for the Config Load is building,
// before any file layer applies. Keyed by the dst pointer so a redundant
// defaults() call inside Load (which builds a different Config) cannot reset
// the real load's values.
func applyEvalDefaults(dst *config.Config) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	setLocked(dst, defaultEvalConfig())
}

// mergeEval applies the [eval] subsection from one config file onto dst's
// tracked entry with per-key granularity: a key absent from THIS file keeps
// the value a lower layer set, matching the core fileLayer's IsDefined
// posture. raw is the file's already-decoded top-level table; the merger
// reads the "eval" subsection from it by name, so the core Config decode
// (which ignores the unknown [eval] table) never has to know it exists.
func mergeEval(md toml.MetaData, raw map[string]any, path string, dst *config.Config) error {
	if !md.IsDefined("eval") {
		return nil
	}
	section, ok := raw["eval"].(map[string]any)
	if !ok {
		return fmt.Errorf("config: [eval] must be a table in %s", path)
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cur := getLocked(dst)
	if md.IsDefined("eval", "strict") {
		b, err := asBool(section["strict"])
		if err != nil {
			return fmt.Errorf("config: [eval].strict in %s: %w", path, err)
		}
		cur.Strict = b
	}
	if md.IsDefined("eval", "min_green") {
		n, err := asInt(section["min_green"])
		if err != nil {
			return fmt.Errorf("config: [eval].min_green in %s: %w", path, err)
		}
		if n < 0 {
			return fmt.Errorf("config: [eval].min_green must be >= 0 (got %d in %s)", n, path)
		}
		cur.MinGreen = n
	}
	setLocked(dst, cur)
	return nil
}

// ConfigFor returns the resolved [eval] values for a *config.Config produced
// by config.Load. When the load defined no [eval] keys, the entry holds the
// built-in defaults (seeded by applyEvalDefaults). A nil cfg or an untracked
// pointer yields the defaults. The eval_run handler calls this with the
// config it loaded so a config edit is honored without a code change.
func ConfigFor(cfg *config.Config) EvalConfig {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if testOverride != nil {
		return *testOverride
	}
	if cfg == nil {
		return defaultEvalConfig()
	}
	if c, ok := cfgByConfig[cfg]; ok {
		return *c
	}
	return defaultEvalConfig()
}

// getLocked returns a copy of dst's tracked entry, seeding defaults if absent.
// Caller holds cfgMu.
func getLocked(dst *config.Config) EvalConfig {
	if c, ok := cfgByConfig[dst]; ok {
		return *c
	}
	return defaultEvalConfig()
}

// setLocked stores v for dst, tracking insertion order and evicting the
// oldest entry when the cap is exceeded. Caller holds cfgMu.
func setLocked(dst *config.Config, v EvalConfig) {
	if _, ok := cfgByConfig[dst]; !ok {
		cfgOrder = append(cfgOrder, dst)
		if len(cfgOrder) > maxTrackedConfigs {
			oldest := cfgOrder[0]
			cfgOrder = cfgOrder[1:]
			delete(cfgByConfig, oldest)
		}
	}
	c := v
	cfgByConfig[dst] = &c
}

// resetConfigForTest clears all tracked config state and any test override.
func resetConfigForTest() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfgByConfig = map[*config.Config]*EvalConfig{}
	cfgOrder = nil
	testOverride = nil
}

// setConfigForTest installs an explicit eval config that ConfigFor returns
// regardless of pointer, so a test can drive the strict gate without writing
// a config file.
func setConfigForTest(c EvalConfig) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	v := c
	testOverride = &v
}

// asBool coerces a raw TOML scalar to a bool, erroring on any other type so a
// mistyped value is loud rather than silently defaulted.
func asBool(v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("want a boolean, got %T", v)
	}
	return b, nil
}

// asInt coerces a raw TOML scalar to an int. BurntSushi decodes bare integers
// into int64 in the generic map, so accept that and narrow.
func asInt(v any) (int, error) {
	switch n := v.(type) {
	case int64:
		return int(n), nil
	case int:
		return n, nil
	default:
		return 0, fmt.Errorf("want an integer, got %T", v)
	}
}
