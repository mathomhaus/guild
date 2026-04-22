# 01 — hello-guild

> *"I just installed guild. How do I make it remember something?"*

You have a house rule: no em dashes in generated text. You also know
there is cleanup work ahead — existing docs almost certainly have
violations. You want every future session to automatically carry both:
the rule (so agents stop producing em-dashes) and the task (so the
cleanup does not fall off your radar). One `lore_inscribe(kind=principle)`
captures the rule. One `quest_post` files the task. Both surface
together on the next `session_start`.

## What this example shows

- `session_start` on a cold project (empty oath, no briefing, no
  bounties — day-one state, not an error)
- `lore_appraise` with zero results — teaches the appraise-before-inscribe
  discipline even when the domain is empty
- `lore_inscribe(kind=principle)` — stores a rule that auto-loads every
  future session
- `quest_post` — files a concrete task to the board
- A second `session_start` — the oath wall carries the new principle AND
  the top bounty surfaces the audit quest; two feedback loops firing
  from a single call

## How to run

1. `./setup.sh` — creates the sandbox project
2. Paste [PROMPT.md](./PROMPT.md) into your MCP-enabled agent (Claude Code, Codex, Cursor, etc.)
3. `./teardown.sh` — cleans up when done

## What to expect

1. First `session_start` returns an empty oath, no bounties, no
   briefing — the cold-start baseline.
2. `lore_appraise "style rules"` returns nothing — confirms no prior
   art exists before writing.
3. `lore_inscribe` creates the principle and returns its LORE-N ID.
4. `quest_post` files the audit task and returns its QUEST-N ID.
5. Second `session_start` shows `1 oath(s) sworn` listing the
   no-em-dashes rule AND the audit quest as the top bounty — two
   feedback loops firing from a single call.

Reference snapshots live in [expected/](./expected/).

## Primitives used

| Primitive | Role |
|---|---|
| `session_start` | Bootstrap session; surface oath + briefing + top bounty |
| `lore_appraise` | Search before writing; normalizes the empty-result case |
| `lore_inscribe(kind=principle)` | Write a rule that auto-loads every future session |
| `quest_post` | File a concrete task; surfaces as top bounty in the next session |

## See also

- [04-session-handoff](../04-session-handoff/) — once principles are in
  place, `quest_brief` hands session state to the next agent
