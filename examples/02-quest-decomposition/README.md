# 02 — quest-decomposition

> *"I need to break a big planning task across parallel subagents without collisions."*

## Scenario

You want to plan a rate limiter. Rather than reasoning through every dimension in a single context, you decompose the work: one parent agent seeds a quest per sub-topic, then fans out four subagents in a single message. Each subagent atomically claims its quest, inscribes one short decision to lore, and fulfills. The parent collects the inscribed decisions and synthesizes them into a single paragraph. No human sits between the subagents — guild's atomic `quest_accept` prevents two agents from grabbing the same work.

This is one demonstrated approach the spike data validates, not a workflow you should adopt. Guild is un-opinionated about process; you compose your own.

## What this example shows

- **Atomic `quest_accept` prevents collisions.** When N subagents launch simultaneously, the first to call `quest_accept(quest_id=X)` wins; the rest see an already-accepted error and move on. No locking, no coordination code.
- **Parent fan-out coordinates without a human between tasks.** One Agent-tool message spawns all four subagents. The parent then waits for all to fulfill before synthesizing — the entire decompose-work-synthesize loop runs unattended.
- **Subagents inscribe findings so the parent can cite them.** Each subagent writes one lore entry — usually `kind=decision` (a committed choice), though list-shaped outputs like *"3 pitfalls to avoid"* naturally land as `kind=research`. The parent reads those entries and combines them into a synthesis inscribe. Guild's lore layer is what makes distributed work composable.
- **`quest_fulfill` cascades unblocks.** When all sub-quests are fulfilled the parent quest's dependency gate opens and the parent completes cleanly.

## Evidence anchor

This pattern has been measured on real planning work and delivered 2–5× fewer tokens than a single-context orchestrator, with equivalent or better quality. The efficiency advantage scales with how much planning depth the task needs.

Full writeups: `~/Documents/projects/gsd-guild-spike-{3,6}-RESULT.md`.

*One shape that works — not the shape to adopt.*

## How to run

### Prerequisites

Requires an MCP client with Agent-tool access for parallel subagent spawn. Confirmed working in Claude Code, Codex, and Cursor. Other MCP clients that do not support parallel Agent-tool calls cannot run this example — this is an intentional harness constraint, not a guild limitation.

### Setup

```bash
cd examples/02-quest-decomposition
./setup.sh
```

`setup.sh` initializes the sandbox project `guild-example-02-quest-decomposition` and pre-seeds the parent quest plus four sub-quests. It does not run any agent work.

### Agent-driven run

Paste [PROMPT.md](./PROMPT.md) into your MCP-enabled agent. The prompt is self-contained: it instructs the parent to accept the pre-seeded quest, fan out four subagents in a single message, synthesize their lore entries, and brief.

There is no `run.sh` for this example. Parallel subagent fan-out is a harness-level capability — it cannot be scripted from the CLI.

### Teardown

```bash
./teardown.sh
```

## What to expect

1. Parent agent calls `guild_session_start`, then `quest_accept(QUEST-1)`.
2. Parent posts a single Agent-tool message spawning SUB-1 through SUB-4 in parallel.
3. Each subagent: `quest_accept` → `lore_appraise` (one call, discipline check) → `lore_inscribe` (one sentence; `kind=decision` for choices, `kind=research` for the pitfalls list) → `quest_fulfill`.
4. After all four subagents complete, the parent reads their lore entries and inscribes a one-paragraph synthesis as `kind=decision`.
5. Parent calls `quest_fulfill(QUEST-1)` then `quest_brief`.

Wall clock: under 5 minutes. Token budget: approximately 200k total across parent and all subagents. Sub-tasks are deliberately toy-sized — the example demonstrates the parallelism mechanism, not realistic planning depth.

See `expected/` for captured snapshots: quest list (all fulfilled), lore catalog (5–6 entries), closing brief.

## Primitives used

`guild_session_start`, `quest_accept`, `lore_appraise`, `lore_inscribe`, `quest_fulfill`, `quest_brief`

## See also

- Prerequisite concept: [01-hello-guild](../01-hello-guild/) for primitive basics
- Grand-tour next step: [05-lore-only](../05-lore-only/) — the minimalist opposite of this
