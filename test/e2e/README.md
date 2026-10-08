# test/e2e: Docker end-to-end harness

Scripted full-loop guild scenarios driven over MCP stdio against an
isolated container. This is the development loop for changes that can
alter in-container behavior: build the image, run the same scenario
suite, diff the recorded transcripts.

## Running

```
make e2e-docker   # build the image, then run the suite against it
make e2e          # run against an already-built image (GUILD_E2E_IMAGE)
make e2e-update   # regenerate golden/ transcripts from a live run
```

The suite is opt-in: without `GUILD_E2E_DOCKER=1` (the make targets set
it) every test skips, so `go test ./...` and `make check` stay fast and
docker-free.

## Isolation

- Each scenario gets a fresh container; all guild state lives in the
  container's own `/home/guild/.guild` and dies with it.
- Containers run with `--network none`: guild needs no network at
  runtime (model assets are embedded, the release update check is
  disabled via `GUILD_NO_UPDATE_CHECK=1`), and the flag proves it.
- The test process swaps `HOME` to an empty canary directory before any
  scenario runs and fails the suite if anything appears there. The host
  `~/.guild` is provably untouched.

## Scenarios

- **baseline**: `guild init`, MCP handshake (`initialize`, newline-
  delimited JSON-RPC over stdio), `tools/list`, `guild_session_start`,
  lore inscribe/appraise round-trip, quest post/accept/fulfill
  round-trip. Asserts on actual tool output text and pins the bytes in
  `golden/baseline.golden`.
- **concurrency**: two parallel MCP stdio sessions inscribing against
  the same container state, then a verification session that requires
  every entry back (the regression net for lost concurrent writes).
  Only the deterministic verification phase is golden-recorded; entry
  id assignment depends on interleaving and is scrubbed. Direct-mode
  only: its repair path is the next process's once-per-process
  auto-backfill, which has no equivalent under a shared long-lived
  daemon (see `GUILD_E2E_MODE` below), so it self-skips in daemon mode.

## Golden transcripts

Scenario output is recorded, run through a scrub pass that replaces
legitimate nondeterminism (timestamps, versions, embedder progress,
durations, platform-dependent onnxruntime warnings) with stable
placeholders, and compared byte-for-byte against `golden/*.golden`.

Byte-equality, not substring checks, is the point: the planned guild
daemon must be observably invisible, and these transcripts are the
yardstick. A daemon-mode run must produce byte-identical scrubbed
transcripts against the same goldens.

When an intentional output change breaks a golden, run `make e2e-update`
and review the transcript diff like any other code change.

## GUILD_E2E_MODE (process model)

`GUILD_E2E_MODE` selects how sessions reach the guild server:

- `direct` (or unset, the default): one `guild mcp serve` process per
  session, served in-process. This is the no-daemon baseline.
- `daemon`: the harness starts an in-container daemon (`guild daemon
  start`) after `guild init` and waits for it to publish a dialable
  socket; every session afterward routes through the shim pipe to that
  daemon. The scrubbed transcripts are compared against the SAME
  goldens, so a passing daemon-mode run mechanically proves the
  "daemon-up equals daemon-down" invariant: the daemon is observably
  invisible. The `baseline` scenario runs in both modes; `concurrency`
  is direct-mode only (see Scenarios).

Anything other than `direct` or `daemon` fails fast.

A finer-grained version of this same parity assertion runs without
Docker (and therefore in CI's standard test job) in
`tests/integration/daemon_parity_test.go`: it runs the canonical MCP and
CLI scenarios daemon-down and daemon-up in two fresh isolated homes and
diffs the scrubbed output byte-for-byte.

## A note on embedding writes

On the MCP surface, vector writes are detached goroutines: a server
that exits immediately after `lore_inscribe` returns can leave rows
pending, to be repaired by the next server process's auto-backfill.
The concurrency scenario synchronizes on full vector coverage (via
`lore_appraise` warmup + `lore_health` polling) before recording its
transcript, so the golden captures the deterministic steady state
rather than a backfill race.
