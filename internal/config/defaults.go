// Package config implements guild's layered configuration loader.
//
// Precedence (low → high):
//  1. Built-in defaults (this file)
//  2. ~/.guild/config.toml      (user-wide)
//  3. <repo>/.guild/config.toml (per-project overrides)
//  4. Environment variables      (GUILD_PROJECT, GUILD_NO_USAGE_LOG, GUILD_NO_EMOJI, GUILD_NO_DAEMON, GUILD_NO_SLEEP, GUILD_NO_WATCH, GUILD_EMBED_BACKEND, GUILD_EMBED_MODEL, GUILD_PROVIDER_BACKEND, GUILD_PROVIDER_MODEL)
//  5. CLI flags                  (via pflag.FlagSet)
//
// Backend selection ([embed].backend, [provider].backend; ADR-006 Phase 4)
// rides the same five layers: the built-in default selects the bundled local
// BGE embedder and a no-op LLM provider, so a silent config is byte-identical
// to pre-Phase-4 behavior. A file key, then GUILD_EMBED_BACKEND /
// GUILD_PROVIDER_BACKEND env, then a flag, override in that order.
package config

// ScoringConfig holds BM25 + recency + title-boost weights used by lore appraise.
type ScoringConfig struct {
	// WFTS is the BM25 full-text-search weight (0.0–1.0).
	WFTS float64 `toml:"w_fts"`
	// WRecency is the recency decay weight (0.0–1.0).
	WRecency float64 `toml:"w_recency"`
	// HalfLifeDays controls recency decay: score halves every N days.
	HalfLifeDays float64 `toml:"half_life_days"`
	// TitleMatchBoost is added to the score when a query exactly matches an entry title.
	TitleMatchBoost float64 `toml:"title_match_boost"`
	// TitleTokenBoost is added to the score when all query tokens appear in the title.
	TitleTokenBoost float64 `toml:"title_token_boost"`
}

// InscribeConfig holds validation thresholds for lore inscribe.
// Keeps principle entries short to preserve oath-wall hygiene.
type InscribeConfig struct {
	// PrincipleMaxWords is the word-count threshold for principle entries;
	// inscribe warns when exceeded (target: principles ≤60 words).
	PrincipleMaxWords int `toml:"principle_max_words"`
	// BloatSevereThreshold is the word count above which lore commune --fix
	// auto-trims bloated principle entries.
	BloatSevereThreshold int `toml:"bloat_severe_threshold"`
	// ValidDays maps an entry kind to the valid_days window stamped onto
	// new entries when the caller does not pass valid_days explicitly.
	// 0 means "never stale" (stored as NULL). Configured per kind:
	//
	//	[inscribe.valid_days]
	//	research = 30
	//	decision = 180
	//
	// Keys must be one of the five canonical kinds (see loreKinds);
	// unknown keys and negative values fail config load. Keys absent
	// from a file keep the value from the layer below (per-key merge,
	// same as every other config knob).
	ValidDays map[string]int `toml:"valid_days"`
}

// loreKinds is the set of canonical entry kinds accepted as keys under
// [inscribe.valid_days]. It mirrors lore.AllKinds(); the list is kept
// local because internal/config sits at the bottom of the dependency
// graph and must not import domain packages (internal/lore deliberately
// does not import internal/config, and the reverse edge would pin the
// two packages together forever). TestValidDaysKindsMatchLore asserts
// this list stays in sync with lore.AllKinds().
var loreKinds = map[string]bool{
	"idea":        true,
	"research":    true,
	"decision":    true,
	"observation": true,
	"principle":   true,
}

// TelemetryConfig controls per-call usage logging to ~/.guild/usage.log.
type TelemetryConfig struct {
	// UsageLog enables writing TSV records to ~/.guild/usage.log.
	// Off by default; set true in [telemetry] config to opt in.
	UsageLog bool `toml:"usage_log"`
}

// SleepConfig controls the daemon's idle dream-pass scheduler: after
// IdleMinutes of no MCP or CLI activity, the resident daemon spends one
// bounded pass on autonomous maintenance (consolidation, echo renewal,
// embed backfill). The scheduler only fires inside `guild daemon run`;
// the no-daemon path never reaches this code.
type SleepConfig struct {
	// Enabled lets the running daemon fire idle dream passes. On by
	// default because daemon.autostart is on from day one: a user who
	// opts out of the daemon entirely never runs this path. Set false in
	// [sleep] config, or pass GUILD_NO_SLEEP=1, to keep the daemon
	// serving but never dreaming.
	Enabled bool `toml:"enabled"`

	// IdleMinutes is how long the daemon must see no MCP request and no
	// CLI exec RPC before a dream pass becomes due. It also gates the
	// gap between passes: a new pass never starts within IdleMinutes of
	// the previous pass ending.
	IdleMinutes int `toml:"idle_minutes"`

	// PassBudgetSeconds is the wall budget handed to each pass. A pass
	// that overruns is cancelled mid-step and journaled as partial, so
	// the daemon never blocks serving on a long pass.
	PassBudgetSeconds int `toml:"pass_budget_seconds"`
}

// EmbedConfig selects the embedding backend by name (ADR-006 Phase 4).
// It is the config surface for the name-keyed embedder registry in
// internal/lore/embed: Backend names a registered EmbedderFactory, Model is
// an optional per-backend model identifier. The default (Backend="local-bge")
// resolves to guild's bundled BGE/ONNX embedder, so a silent config is
// byte-identical to the pre-Phase-4 behavior. internal/config deliberately
// does NOT import internal/lore/embed (it sits at the bottom of the
// dependency graph); the adapter layer translates this struct into an
// embed.EmbedConfig at the wiring call site.
type EmbedConfig struct {
	// Backend is the registry name selecting which embedder to build:
	// "local-bge" (default, the bundled BGE path), or an alternate
	// registered backend. Empty is treated as the default. An unknown
	// name is rejected at construction by BuildEmbedder, not here, so a
	// config that names a backend a future build will register still loads.
	Backend string `toml:"backend"`

	// Model is an optional model identifier passed to the selected backend
	// factory (e.g. an OpenAI/Ollama model name). The local BGE backend
	// ignores it: its model is pinned by the bundled manifest. Empty is
	// always valid.
	Model string `toml:"model"`
}

// ProviderConfig is the LLM provider/model selection seam (ADR-006 Phase 4,
// deliverable 5). It is the config surface a future LLM-calling module (e.g. a
// sleep/compression module) will read to pick a provider+model by name from
// the provider registry in internal/llm. No live LLM dependency or client is
// wired in this phase: this struct plus the registry seam is the whole
// surface. The default (Backend="noop") resolves to a registered stub that
// never calls a network, so a silent config makes no provider call.
type ProviderConfig struct {
	// Backend names a registered LLM provider (e.g. "noop" today; "openai",
	// "anthropic", "ollama" when a future module adds them). Empty resolves
	// to the default stub. Selection is by name through the registry, the
	// same idiom as [embed].backend.
	Backend string `toml:"backend"`

	// Model is the model identifier handed to the selected provider (e.g. a
	// chat-model name). Empty defers to the provider's own default. The stub
	// ignores it.
	Model string `toml:"model"`
}

// DaemonConfig controls the optional background daemon.
type DaemonConfig struct {
	// Autostart lets the first MCP shim that finds no running daemon
	// spawn one (under a lock so concurrent shims elect a single
	// spawner). On by default so the background daemon is available
	// without an explicit start; set false in [daemon] config, or pass
	// GUILD_NO_DAEMON=1 / --no-daemon, to keep the no-daemon path. With
	// it off the shim never spawns and the process behaves exactly as a
	// build without daemon support: probe-and-fall-through only.
	Autostart bool `toml:"autostart"`

	// Watch lets the running daemon watch every registered project root
	// for file and git activity and turn it into lore staleness signals
	// and capped renewal quests within seconds. On by default for the
	// same reason as autostart: the watcher only runs inside a daemon,
	// and opting out of the daemon already disables it. Set false in
	// [daemon] config, or pass GUILD_NO_WATCH=1, to keep the daemon
	// serving (and still dreaming) but never watching; staleness then
	// falls back to the query-time check, exactly as the no-daemon path.
	Watch bool `toml:"watch"`

	// RenewalCapPerPass bounds how many renewal quests the watcher posts
	// per debounced event batch, across all entries that batch flagged.
	// Dedupe still suppresses a second open renewal quest for an entry,
	// so the cap only limits a burst; entries left over are picked up on
	// the next event or the idle dream pass. Non-positive means post
	// nothing (the watcher records signals but mints no quests).
	RenewalCapPerPass int `toml:"renewal_cap_per_pass"`

	// WatchDebounceMS is the quiet window, in milliseconds, the watcher
	// waits after the last raw filesystem event before emitting one
	// normalized event. Non-positive uses the watcher's built-in default
	// (one second), which coalesces an editor's atomic-save burst into a
	// single event.
	WatchDebounceMS int `toml:"watch_debounce_ms"`

	// LeaseTTLSeconds is how long a quest lease stays valid without a
	// heartbeat. The daemon's renewal tick refreshes a live session's
	// leases well inside this window; a crashed agent stops heartbeating
	// and its lease lapses one TTL later, after which a reaper can forfeit
	// the stale claim. Deliberately generous relative to the heartbeat
	// interval so several missed ticks never expire a live session. Only
	// the daemon reads it; the no-daemon path writes no lease rows.
	// Non-positive falls back to the built-in default.
	LeaseTTLSeconds int `toml:"lease_ttl"`

	// HeartbeatIntervalSeconds is the cadence at which the daemon's
	// renewal tick sweeps every live session and refreshes its leases.
	// Kept well below LeaseTTLSeconds so a single missed beat never
	// expires a live lease. Only the daemon reads it. Non-positive falls
	// back to the built-in default.
	HeartbeatIntervalSeconds int `toml:"heartbeat_interval"`

	// ReapIntervalSeconds is the cadence at which the daemon's lease
	// reaper sweeps for expired leases and auto-forfeits the zombie claim
	// behind any whose session has gone (the agent crashed), returning the
	// work to the board. It scans task_leases only, so a claim accepted
	// without the daemon is never touched. Only the daemon reads it.
	// Non-positive falls back to the built-in default.
	ReapIntervalSeconds int `toml:"reap_interval"`
}

// Config is the merged, validated configuration for a guild process.
// All fields are safe to read after Load returns; nil pointer dereferences
// cannot happen because Load always fills in defaults before returning.
type Config struct {
	// Project is the active project name.  Resolved from: --project flag →
	// GUILD_PROJECT env → per-project config → session state file.
	// Empty string means "not yet resolved" (MCP server sets it later via
	// guild_session_start).
	Project string `toml:"project"`

	// NoUsageLog is the merged runtime disable bit for usage logging.
	// True whenever Telemetry.UsageLog is false (the default) or GUILD_NO_USAGE_LOG=1 is set.
	NoUsageLog bool `toml:"-"`

	// NoEmoji substitutes ASCII equivalents for emoji prefixes in all output.
	// Driven by GUILD_NO_EMOJI=1 env or --no-emoji flag.
	NoEmoji bool `toml:"-"`

	Scoring   ScoringConfig   `toml:"scoring"`
	Inscribe  InscribeConfig  `toml:"inscribe"`
	Telemetry TelemetryConfig `toml:"telemetry"`
	Daemon    DaemonConfig    `toml:"daemon"`
	Sleep     SleepConfig     `toml:"sleep"`

	// Embed selects the embedding backend by name (ADR-006 Phase 4). The
	// default backend is the bundled local BGE/ONNX embedder, so a silent
	// config is byte-identical to pre-Phase-4 behavior. See EmbedConfig.
	Embed EmbedConfig `toml:"embed"`

	// Provider is the LLM provider/model selection seam (ADR-006 Phase 4,
	// deliverable 5). The default backend is a no-op stub that makes no
	// network call; a future LLM-calling module reads this to pick a
	// provider by name. See ProviderConfig.
	Provider ProviderConfig `toml:"provider"`

	// Modules is the [modules] toggle table (ADR-006 Phase 3): a map from
	// a capability module's Name() to an explicit on/off bit. A key absent
	// from the map means "use the module's own DefaultEnabled()"; a present
	// key overrides it in either direction. The map is the merged result of
	// (in precedence order) a [profile] preset baseline, the [modules] file
	// keys, GUILD_MODULE_<NAME> / GUILD_NO_<NAME> env, and --module /
	// --no-<name> flags. Empty by default so a silent config leaves every
	// module on its own default, the byte-identical pre-Phase-3 behavior.
	// Resolve a final verdict through ModuleEnabled, never by reading this
	// map directly.
	Modules ModulesConfig `toml:"modules"`

	// Profile carries the [profile] preset name (ADR-006 Phase 3). A
	// preset expands into a Modules baseline BEFORE the file/env/flag
	// layers apply, so an explicit [modules] key always wins over the
	// preset it sits inside. Empty means no preset (every module on its
	// own default).
	Profile ProfileConfig `toml:"profile"`
}

// ModulesConfig is the [modules] toggle table: module name to explicit
// enabled bit. Decoded directly from TOML (a table of booleans) and also
// the merge target for the preset, env, and flag layers.
type ModulesConfig map[string]bool

// ProfileConfig is the [profile] section. Today it carries only the
// preset name; future profile-scoped knobs (default backends, etc.) land
// here alongside it.
type ProfileConfig struct {
	// Preset names a bundle of module toggles expanded into Modules before
	// lower layers apply. See presets.go for the registry. An unknown name
	// fails config load (a typo must be loud, not silently ignored).
	Preset string `toml:"preset"`
}

// defaults returns a Config populated with the built-in baseline values.
// All other layers in Load apply deltas on top of this.
func defaults() Config {
	cfg := baseDefaults()
	// Seed every registered module's built-in config values before any
	// file layer applies, so a partial override keeps the module's
	// defaults for absent keys (ADR-006 Phase 3).
	applyModuleDefaults(&cfg)
	return cfg
}

// baseDefaults holds the kernel's built-in baseline. Split from defaults()
// so module-default seeding has a clean target and tests can compare
// against the pure kernel baseline.
func baseDefaults() Config {
	return Config{
		Scoring: ScoringConfig{
			WFTS:            0.7,
			WRecency:        0,
			HalfLifeDays:    30,
			TitleMatchBoost: 1.0,
			TitleTokenBoost: 0.5,
		},
		Inscribe: InscribeConfig{
			PrincipleMaxWords:    60,
			BloatSevereThreshold: 120,
			// Built-in decay windows: research and decision entries fade;
			// idea/observation/principle never auto-stale (0 = never stale).
			// Must match the kindValidDays fallback in internal/lore so
			// zero-config behavior is byte-identical with or without this
			// map threaded through.
			ValidDays: map[string]int{
				"idea":        0,
				"research":    30,
				"decision":    180,
				"observation": 0,
				"principle":   0,
			},
		},
		Telemetry: TelemetryConfig{
			UsageLog: false,
		},
		Daemon: DaemonConfig{
			// On from day one: the first shim spawns the daemon when
			// none is running (ollama pattern). GUILD_NO_DAEMON=1, the
			// --no-daemon flag, or [daemon] autostart = false opt out and
			// restore the byte-identical no-daemon path.
			Autostart: true,
			// Watch on by default so a daemon-on install gets
			// event-driven staleness for free. GUILD_NO_WATCH=1 or
			// [daemon] watch = false keeps the daemon serving without a
			// watcher goroutine. Three renewal quests per event batch is
			// the same conservative per-pass cap the idle scheduler uses:
			// a burst drains over several events instead of flooding the
			// board. Zero debounce uses the watcher's one-second default.
			Watch:             true,
			RenewalCapPerPass: 3,
			WatchDebounceMS:   0,
			// Ten-minute lease TTL with a thirty-second heartbeat: the
			// daemon refreshes a live session's leases every interval, so
			// the TTL is deliberately generous (twenty heartbeats) and a
			// burst of missed ticks under load never expires a live
			// session. A crashed agent stops heartbeating and its lease
			// lapses one TTL later for a reaper to forfeit.
			LeaseTTLSeconds:          600,
			HeartbeatIntervalSeconds: 30,
			// Sweep for expired leases once a minute: a crashed agent's
			// claim returns to the board within about one TTL plus one
			// sweep, and the scan over expired rows only is negligible load.
			ReapIntervalSeconds: 60,
		},
		Sleep: SleepConfig{
			// On by default for the same reason as daemon.autostart:
			// the scheduler only runs inside a daemon, and opting out of
			// the daemon (GUILD_NO_DAEMON / --no-daemon) already disables
			// it. GUILD_NO_SLEEP=1 or [sleep] enabled = false keeps the
			// daemon serving but never dreaming.
			Enabled: true,
			// Ten idle minutes before a pass, with a sixty-second wall
			// budget per pass: the ADR cost note requires that dreaming
			// never makes a waking session feel slower, so passes stay
			// short and new activity preempts them.
			IdleMinutes:       10,
			PassBudgetSeconds: 60,
		},
		Embed: EmbedConfig{
			// "local-bge" is the bundled BGE/ONNX embedder, the same
			// embedder guild builds today. The default MUST stay this so a
			// config with no [embed] section yields byte-identical
			// embeddings and wiring (ADR-006 Phase 4 parity bar). Model is
			// empty: the local backend pins its model via the bundled
			// manifest and ignores this field.
			Backend: "local-bge",
			Model:   "",
		},
		Provider: ProviderConfig{
			// "noop" is the registered stub provider: it makes no network
			// call and is the default so a silent config never reaches an
			// LLM. A future LLM-calling module flips this to a real
			// provider name. Empty model defers to the provider default.
			Backend: "noop",
			Model:   "",
		},
	}
}
