# Runtime defaults and upgrades

Guild keeps its SQLite state locally. macOS and Linux release binaries include the local
BGE embedding model. Windows and builds without embedding assets use keyword
search; a `go install` build does not include those assets. The optional Ollama backend sends source and query text to
its configured HTTP endpoint, which defaults to `127.0.0.1:11434` and can be
changed with `GUILD_OLLAMA_BASE_URL`.

## Background daemon and watcher

The first MCP connection starts a daemon automatically when no daemon is
running. The daemon watches registered project roots by default. File and git
changes can create staleness signals and renewal quests; the default renewal
limit is three quests per debounced event batch. These quests ask an agent or
person to review the knowledge. The watcher does not automatically seal,
reforge, or rewrite the cited entry.

The daemon also renews leases for connected sessions and can return an expired
claim to the board after its session disappears. The default lease lasts ten
minutes, with a thirty-second heartbeat and a one-minute reaper interval.
Claims accepted without daemon lease wiring are not included in that sweep.

Use `GUILD_NO_WATCH=1` or `[daemon] watch = false` to disable event-driven
watching. Use `GUILD_NO_SLEEP=1` or `[sleep] enabled = false` to disable sleep
passes. These settings affect new processes: stop and restart an existing
daemon to apply changed settings to it.

`GUILD_NO_DAEMON=1` or `[daemon] autostart = false` prevents automatic daemon
startup. It does not stop an existing daemon or bypass a matching resident
daemon. Stop the daemon first when testing or using the direct MCP process
path. Direct mode can still start a sleep autopass unless sleep is disabled
separately.

## Maintenance status

The sleep scheduler and direct-mode autopass currently record completed passes
with no maintenance steps. Consolidation, renewal, and embedding step
implementations are tested but are not registered in the production step
registry. Enabling sleep therefore does not yet run those steps automatically.
The file watcher and embedding startup repair work independently of this
registry.

This is a feature-completeness limitation, not permission to enable automatic
consolidation during an upgrade. Registering those steps would introduce new
unattended mutations and needs its own review and production-path validation.

## Optional capabilities

The `eval`, `compression`, and `observability` modules are off by default. They
can be enabled through `[modules]` or the `GUILD_MODULE_<NAME>` environment
variables. For example, `GUILD_MODULE_EVAL=1` enables the `eval_run` MCP tool and
`guild eval run`; `--module eval=true` enables it for a CLI invocation.

Compression retrieval uses a process-local memory cache with a five-minute
TTL. A lossy compression marker requires the same running server's cache and
is not a durable reference across restarts. Lossless JSON compaction does not
require that cache.

Enabling observability starts its daemon service. Its default metrics address
is `127.0.0.1:9090`, which listens locally. Set an explicit address such as
`:9090` to allow remote scrapes, or an empty string to disable the HTTP endpoint
while retaining event logging. The endpoint exposes aggregate daemon counters
and gauges without authentication; it does not expose entry text or project
labels.

Alternate embedding-model initialization remains experimental. Existing BGE
databases do not transition to a different Ollama model through configuration
alone: the selected model and stored identity differ, so index wiring safely
falls back to keyword search. Use the bundled default backend for a normal
upgrade; model transitions need a supported identity and repair workflow.

Lifecycle hooks are installed explicitly with `guild hooks install`. `--dry-run`
previews installation without writing settings. `guild hooks sync` preserves
foreign hooks and unrelated settings while updating Guild-owned groups.

## Database upgrades and rollback

Follow the [retrieval upgrade procedure](retrieval-upgrade.md) before the first
new startup. Stop every old MCP process and daemon, back up both databases and
their SQLite sidecar files, then restart with the new binary. Migration 014
preserves canonical quest records and notes but rebuilds the project-qualified
search bridge and discards old quest vectors. Keyword search remains available
while vectors are repaired.

Restoring the corresponding pre-upgrade database backup is required when
rolling back to an older binary. Replacing only the executable is not a
supported database rollback. Keep old and upgraded stores separate until the
new startup and searches have been verified.
