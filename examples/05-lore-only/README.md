# 05 — lore-only

> *"I just want guild as a notebook — no quest ceremony."*

After a CI debugging session, you have three things worth remembering: what you
observed (7-minute builds, 60% on npm install), what you decided (add an npm
cache keyed on package-lock.json), and a rough idea for later (evaluate bun for
install speed). None of these need a quest. You reach for `lore_inscribe` three
times — once per kind — then `lore_appraise "ci"` pulls all three back clustered
by topic in one shot.

## What this example shows

- Guild works as a pure memory layer. You never have to touch quest_post,
  quest_accept, or quest_fulfill. The lore axis stands alone.
- `lore_inscribe` with three distinct kinds (`observation`, `decision`, `idea`)
  covers the natural progression from "I noticed X" to "we chose Y" to "someday Z".
- A single `lore_appraise` query retrieves all three entries clustered by topic,
  whether the same agent wrote them or three different agents in three different
  sessions.
- Oath and cross-project search still work in minimalist mode -- guild's full
  retrieval surface is always on, even when you're using only one verb.

There's also `kind=research`, for substantive investigations where you've surveyed
several approaches and documented tradeoffs. Not shown here because casual
note-taking isn't quite the right shape for it — see autogen docs for the full
kind taxonomy.

## How to run

1. `./setup.sh` — creates the sandbox project
2. Paste [PROMPT.md](./PROMPT.md) into your MCP-enabled agent (Claude Code, Codex, Cursor, etc.)
3. `./teardown.sh` — cleans up when done

## What to expect

1. Three `lore_inscribe` calls succeed in sequence -- one `observation`, one
   `decision`, one `idea` -- all tagged `topic=ci`.
2. `lore_appraise "ci"` returns all three as a cluster. No quest state is created
   at any point.

Reference snapshots live in [expected/](./expected/).

## Primitives used

| Primitive | Role |
|---|---|
| `session_start` | Bootstrap session; set active project |
| `lore_inscribe(kind=observation)` | Capture a raw observation before it evaporates |
| `lore_inscribe(kind=decision)` | Record what was decided and why |
| `lore_inscribe(kind=idea)` | Preserve a low-priority idea without promoting it to a task |
| `lore_appraise` | Retrieve the full cluster by topic |

## See also

- [01-hello-guild](../01-hello-guild/) -- if you want a rule that auto-loads
  every session, use `kind=principle` instead
- [04-session-handoff](../04-session-handoff/) -- once you do want task tracking,
  `quest_brief` hands session state to the next agent
