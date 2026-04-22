# 03 — cross-project

> *"We already solved this in project A — how does project B's agent know that?"*

Six months ago, working on project A, you made a decision: exponential backoff, 100ms base, max 3
retries, to avoid thundering-herd failures against flaky dependencies. That decision lives in
project A's lore. Today, starting fresh on project B, your agent is about to re-derive a retry
policy from scratch — unless it knows to look.

Project-scoped memory tools (per-repo markdown, per-session caches, tool-specific context files)
cannot answer this question. They are designed to isolate projects. Guild's lore is stored at the
user install level (`~/.guild/*.db`), so a single `lore_appraise(all_projects=true)` call lets any
agent search across every project the user has ever registered. When project B's agent finds the
prior art, it inscribes its own decision with `--informs` pointing at A's entry — creating a typed
provenance edge that spans project boundaries and persists for future agents.

The knowledge graph grows with you, not with any one project.

## What this example shows

- `lore_appraise(query=..., all_projects=true)` — surfaces prior art from any registered project,
  not just the active one
- `lore_study(entry_id=...)` — pulls the full entry from project A so the agent can read it before
  deciding
- `lore_inscribe(kind=decision, ..., informs=[A's-LORE-N])` — records the new decision in project
  B with an explicit edge to the prior art that informed it
- The resulting `LINKED ENTRIES` block in `lore study` proves the graph spans projects

## How to run

### 1. Set up the sandboxes

```bash
./setup.sh
```

Creates `/tmp/guild-example-03-project-a` (one pre-seeded decision) and
`/tmp/guild-example-03-project-b` (empty).

### 2. Run the agent

Paste [PROMPT.md](./PROMPT.md) into your MCP-enabled agent. The agent should be working in
project B's context.

### 3. Verify the edge

After the agent finishes, run:

```bash
guild lore study <B-entry-id>   # ID printed by agent's inscribe call
```

The `LINKED ENTRIES` block will show `← LORE-N [informs] Exponential backoff retry policy`.

### 4. Tear down

```bash
./teardown.sh
```

## What to expect

1. Agent calls `guild_session_start` in project B context — sees empty lore.
2. Agent calls `lore_appraise(query='retry backoff', all_projects=true)` — finds A's entry.
3. Agent calls `lore_study` on A's entry — reads full summary and metadata.
4. Agent calls `lore_inscribe` in project B with `informs=[A's-entry-id]` — new entry created.
5. `guild lore study` on B's new entry shows the cross-project provenance edge.

See `expected/` for real snapshots from the reference run.

## Primitives used

| Primitive | Role |
|---|---|
| `guild_session_start` | Bootstrap session in project B |
| `lore_appraise(all_projects=true)` | Search every project for prior art, not just the active one |
| `lore_study(entry_id=N)` | Read the full entry + its linked provenance |
| `lore_inscribe(..., informs=[N])` | Create a new decision with a typed edge to its source |

## See also

- Prerequisite: [01-hello-guild](../01-hello-guild/) — inscribe/appraise within a single project
- [02-quest-decomposition](../02-quest-decomposition/) — quest ceremony and task decomposition
- [04-session-handoff](../04-session-handoff/) — briefing across sessions
