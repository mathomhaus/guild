# ADR-006 — Module SDK: a config-toggleable capability platform

- Status: Proposed
- Date: 2026-06-13
- Supersedes/extends: ADR-003 (embedding pipeline ports), ADR-005 (daemon)
- Campaign: `module-sdk`
- Spec of record: this document + the linked `kind=decision` lore entry

## Context

The agent-tooling landscape is converging on a recurring set of capabilities:
durable daemon, MCP surface, provider proxy, observability, evals/benchmarks,
hosted docs, and context compression. guild already owns three of the hardest
pillars — tasks (`quest`), durable memory (`lore`), and cross-session context
(`session`) — and a daemon that supervises them. We want guild to be able to
absorb the rest **as native Go capabilities** without each new capability
forcing edits across the whole tree, and we want the operator to **turn
capabilities on and off from config** ("disable compression", "use embedder
X").

Today guild is *moderately extensible for new verbs but not for new domains*.
The blocking facts, all verified in-tree:

1. **No self-registration for tools.** Every verb is hand-listed twice — once in
   `internal/mcp/register.go:135` (`registerAlwaysOn`, ~40 `BindMCP` lines) and
   once in a cobra `init()` (`internal/cli/quest.go:159`). There is no registry;
   adding a domain means editing both static lists.
2. **No storage isolation.** `internal/storage/migrate.go:56` applies one flat,
   numbered `migrations/*.up.sql` corpus; the `scope` argument is a log label
   only (`migrate.go:100`), so `001_init.up.sql` creates both lore *and* quest
   tables in *both* databases. A new domain cannot own its own schema or DB.
3. **God-struct accretion.** `command.Deps` (`internal/command/command.go:83`)
   grows one field per cross-cutting capability, two of them typed `any`
   (`Embed`, `Lease`) purely to dodge import cycles, with load-bearing nil
   guards. The daemon's `Config` (`internal/daemon/server.go`) grows one typed
   field + one `if cfg.X != nil { go ... }` per background loop.
4. **Closed enums / bespoke tools.** Lore `Kind` is a closed enum;
   `session_start` is 529 lines outside the `Command` abstraction.

Two things already in-tree are the templates for the fix, and the design leans
on them rather than inventing new machinery:

- `command.Command[I,O]` (`internal/command/command.go:20`): one declarative spec
  already generates **both** the CLI verb and the MCP tool (schema reflected from
  the input struct). Adding a *tool* is genuinely cheap; the gap is *wiring*, not
  authoring.
- `hooks/adapters.Register()` (`internal/hooks/adapters/registry.go:27`): a real
  `init()`-based self-registration registry (the `database/sql` driver pattern,
  panics on dup). This is the only true plugin registry in the codebase. We
  generalize it from harness-adapters to **capability modules**.

The config layer is also already the right shape: `config.Load`
(`internal/config/config.go:298`) is a 5-layer TOML merge
(defaults → `~/.guild/config.toml` → `<repo>/.guild/config.toml` → env → flags)
with per-key `IsDefined` granularity and an established `GUILD_NO_*` disable
convention. We extend it; we do not replace it.

## Decision

Refactor guild into a **kernel + capability modules**, all compiled into the one
local-first binary, with **config deciding which modules are active** and a
**name-keyed backend registry** making models/embedders/stores swappable. No
out-of-process or cross-language plugins in this ADR (see Deferred).

### 1. The `Module` interface

A module is a Go package that self-registers via `init()` (the adapters
pattern). It contributes everything a capability needs across all four surfaces
(CLI, MCP, daemon, storage) and the agent-facing contract:

```go
// internal/module/module.go
type Module interface {
    // Name is the config key and the stable identifier, e.g. "quest".
    Name() string
    // DefaultEnabled reports whether the module is active when config is
    // silent. Core modules (quest, lore, session) default true; heavy
    // capabilities (compression) default false.
    DefaultEnabled() bool
    // Commands returns the module's verbs. Each already generates a CLI
    // command and/or an MCP tool via command.Registrant.
    Commands(reg Capabilities) []command.Registrant
    // Migrations returns the module's own embedded migration FS and the
    // logical database it owns (DBName), so each module's schema is
    // isolated. Return (nil, "") for a module with no storage.
    Migrations() (fs fs.FS, dbName string)
    // Services returns daemon background loops to run when the module is
    // enabled AND the daemon is up.
    Services(reg Capabilities) []daemon.Service
    // Instructions returns the module's fragment of the MCP INSTRUCTIONS
    // contract, included only when the module is enabled. Empty string for
    // none.
    Instructions(cfg ModuleConfig) string
}

func Register(m Module) // panics on empty/dup name, like adapters.Register
func Enabled(cfg *config.Config) []Module // registered ∩ config-enabled, deterministic order
```

`quest`, `lore`, and `session` are **migrated to be the first three modules**,
dogfooding the SDK. New capabilities (`observability`, `eval`, `compression`)
are new modules. The kernel never imports a module; it walks the registry.

### 2. Kernel wiring (replaces the hand-lists)

At startup the kernel:

1. Loads config (`config.Load`).
2. Computes `module.Enabled(cfg)`.
3. For each enabled module: applies its migrations to its DB
   (`storage.Migrate(ctx, db, moduleFS)`), binds its `Commands` onto the active
   surface (CLI tree or MCP server), registers its daemon `Services` (if the
   daemon is running), and appends its `Instructions` fragment.

`registerAlwaysOn` collapses from ~40 hand-written lines to one loop over
`module.Enabled(cfg)`. A disabled module is *absent everywhere*: its tools never
appear on the MCP surface, its CLI verbs never mount, its daemon loops never
start, its instruction fragment is not in the contract the agent reads, and
(optionally) its migrations are not applied.

### 3. Config spine — how a toggle works end to end

One `[modules]` table plus one per-module section. Extends the existing TOML
merge; each module registers its own config sub-struct + merge so `fileLayer`
stops growing a hand-coded `IsDefined` branch per key.

```toml
[modules]
quest         = true
lore          = true
compression   = false      # whole capability off
observability = true

[embed]
backend = "local-bge"      # name-keyed: "local-bge" | "onnx" | "openai" | "ollama"
model   = "bge-small-en-v1.5"

[compression]
strategies = ["json", "diff", "log"]   # which compressors are live
ccr_ttl    = "5m"

[observability]
prometheus   = true
metrics_addr = "127.0.0.1:9090"

[profile]
preset = "developer"       # bundles toggles + backends (Headroom's agent-90/balanced)
```

`module.Enabled` resolves `[modules].<name>` over `DefaultEnabled()`. Each
`GUILD_<MODULE>` / `--no-<module>` env+flag override follows the established
`GUILD_NO_*` pattern. Presets are named bundles that expand into the same
fields before lower layers apply (so explicit keys still win).

### 4. Backend registry — "configure everything"

Headroom's `Protocol`-ports + `EXTERNAL`/`*_backend_name` pattern, expressed
idiomatically in Go: an interface + a `map[string]Factory` populated by `init()`.
Selection is by config name.

```go
type Embedder interface { /* existing internal/lore/embed.Embedder */ }
type EmbedderFactory func(cfg EmbedConfig) (Embedder, error)
func RegisterEmbedder(name string, f EmbedderFactory) // init-time
func BuildEmbedder(cfg EmbedConfig) (Embedder, error) // looks up cfg.Backend
```

guild's existing `lore/embed` interfaces (`Embedder`, `VectorCorpus`,
`CorpusSchema`) already prove this works; we formalize the registry and extend
it to providers/models and compressor strategies. This is the layer that
delivers the "configure models, embedder, compressors" experience.

### 5. Capability registry — retiring the god-structs

`command.Deps`'s `any`-typed cross-cutting fields (`Embed`, `Lease`,
`EvaluateHints`, …) and the daemon `Config`'s per-service fields are replaced by
a typed `Capabilities` registry the kernel constructs from enabled modules and
passes into `Commands`/`Services`. Modules *publish* capabilities (e.g.
`embed` publishes an `Embedder`) and *consume* them by typed key, removing the
import-cycle-driven `any` and the nil-guard fragility. `daemon.Service` becomes a
small `Start(ctx) error` / `Stop()` interface, so the daemon's `Run` loop is a
range over registered services instead of a fixed `if cfg.X != nil` ladder.

## What we port from Headroom, and where

Code-grounded inventory (Headroom checkout, `~/Documents/projects/headroom`).
guild's `lore`+`embed` already *is* Headroom's memory+embedder layer, so we do
not re-port that — we port the *patterns around it*.

| Capability | Home module | Effort | Note |
| --- | --- | --- | --- |
| Decision-gate value objects (auditable "why did it act") | kernel pattern | S | Apply to daemon lease/autopass/staleness decisions first |
| Observability triad (Prometheus + JSONL log + durable rollups) | `observability` | S | The daemon needs this regardless |
| Daemon-as-installed-service (systemd/launchd, graceful drain) | `session`/daemon | S | guild already has a daemon |
| CCR reversible store + retrieval markers | `compression` | M | Also powers a compact `lore_dossier` + `retrieve(hash)` |
| JSON/SmartCrusher, Diff, Log, Search compressors (pure-algorithmic) | `compression` | M–L | Rust source is the spec; off by default |
| CodeAware/AST compressor | `compression` | M | `go-tree-sitter` exists |
| Adversarial eval grid + golden-fixture parity | `eval` | M | Proves recall/ranking isn't gamed by poisoned lore |

## Deferred (explicitly out of scope of this ADR)

- **Provider proxy.** A transparent HTTP proxy in front of Anthropic/OpenAI is a
  *different runtime surface* than tools+memory and collides with the
  "integrate via MCP only" boundary (LORE-475). It is a separate, deliberate
  identity decision, not folded in here. Compression therefore ships as a
  callable tool/module, not a wire proxy, in this ADR.
- **ML-model compressors** (Kompress/ModernBERT, Magika detection). 600MB ONNX +
  cgo. Use cheaper signals (BM25, extension/regex routing) or compose
  out-of-process; do not drag a transformer into the native binary.
- **Out-of-process / cross-language plugins.** Modules are compile-time Go in
  this ADR. The `Module` boundary is designed so a future daemon-socket plugin
  tier (MCP-spoken, consistent with LORE-475) can be added without reshaping
  modules.

## Compatibility & parity contract

This is a refactor; default behavior must not change. The bar matches ADR-005:

- With the default module set (quest+lore+session enabled, everything new off),
  the CLI and MCP surfaces are **byte-identical** to pre-refactor: same tool
  list, same schemas, same output, same DB effects. Golden e2e transcripts
  (`test/e2e/golden/`) are the oracle and must not move on the default path.
- `daemon-up == daemon-down` parity (the existing `GUILD_E2E_MODE` matrix) holds.
- Each module's migrations are additive and forward-only; existing `~/.guild`
  databases upgrade in place with no destructive change.

## Phased rollout (→ quests under campaign `module-sdk`)

- **Phase 1 — Module SDK foundation.** `internal/module` (interface, registry,
  `Enabled`), generalize `storage.Migrate` to take an `fs.FS`, kernel loop in
  `register.go`/CLI. No new capability; pure plumbing. Parity: byte-identical.
- **Phase 2 — Migrate quest/lore/session to modules.** Convert the three pillars
  to `Module` implementations; per-module migration namespaces; capability
  registry replaces the `Deps` `any` fields. Parity: byte-identical.
- **Phase 3 — Daemon `Service` interface + config-toggle plumbing.** Generalize
  daemon `Config` to a service registry; `[modules]` table + per-module config
  registration + presets. Prove a toggle (disable a core module → its surface
  vanishes) in docker.
- **Phase 4 — Backend registry.** Formalize `RegisterEmbedder`/provider/model
  registries; wire `[embed].backend` selection end to end.
- **Phase 5 — `observability` module.** Prometheus + JSONL event log + durable
  rollups on the daemon; adopt decision-gate value objects for daemon decisions.
- **Phase 6 — `eval` module.** Adversarial grid + golden-fixture parity over
  recall/ranking.
- **Phase 7 — `compression` module (off by default).** CCR store + markers +
  pure-algorithmic compressors; `lore_dossier` compaction.

Each phase is one or more PRs to the fork, each adversarially verified, each
green on `make check` + `make e2e-docker`. The proxy and ML pieces are not in
this sequence.

## Testing & prod-safety

- Gate: `make check` (fmt+vet+lint+sqlcheck+test-race+docs-check) and
  `make e2e-docker` (both `GUILD_E2E_MODE=direct` and `=daemon`).
- All build/test runs in Docker and git worktrees. **No `make install`**, the
  daemon never runs against the real `~/.guild`, and nothing pushes to
  `upstream/mathomhaus`, without a separate explicit go-ahead.

## Consequences

- (+) Adding a capability tomorrow = one Go package implementing `Module` + a
  blank-import + a config stanza; CLI/MCP/daemon/schema/contract all extend
  automatically.
- (+) The module system *protects* the core identity: heavy capabilities ship
  off-by-default, so a user who wants only tasks+memory still gets focused guild.
- (+) Retires three accretion hotspots (`Deps`, daemon `Config`, the dual
  registration lists) and the shared-migration foot-gun.
- (−) Net new indirection (registry, capability lookup) the kernel must own;
  mitigated by keeping the three pillars as the reference modules.
- (−) Migrating bespoke `session_start` into the module/command shape is real
  work (529 lines); Phase 2 carries it.
- Risk: parity regressions during the quest/lore/session migration. Mitigated by
  the byte-identical golden-transcript bar and the daemon-up==down matrix.
