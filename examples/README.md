# Examples

Each example shows one thing guild can do for a specific user need. Mix and match however your own process demands — guild is un-opinionated about *how* you work; these examples show *what it can do*.

## Minimum bootstrap

To get value from guild, you need two calls: `session_start` at the beginning, `quest_brief` at the end. Everything between is opt-in.

## What do you want to do?

| I want to... | See |
|---|---|
| Get started after installing guild | [01-hello-guild](./01-hello-guild/) |
| Break a big planning task across parallel subagents without collisions | [02-quest-decomposition](./02-quest-decomposition/) |
| Pull context from a previous project into a new one | [03-cross-project](./03-cross-project/) |
| Hand off mid-task to tomorrow-me (or another agent) | [04-session-handoff](./04-session-handoff/) |
| Use guild as a lightweight memory layer without quest ceremony | [05-lore-only](./05-lore-only/) |

## The grand tour

Each example stands alone — read them in any order. But if you want to see how guild's value compounds as you adopt more of it, read in this order:

1. **[01 hello-guild](./01-hello-guild/)** — guild remembers things within a session
2. **[04 session-handoff](./04-session-handoff/)** — ...and across time
3. **[03 cross-project](./03-cross-project/)** — ...and across projects
4. **[02 quest-decomposition](./02-quest-decomposition/)** — ...and can coordinate multiple agents at once
5. **[05 lore-only](./05-lore-only/)** — ...or strip it back to just a notebook, if that's all you need

Each arrow adds one axis of value; none require the previous example as a prerequisite.

## How each example is structured

Every example directory contains:

- **`README.md`** — scenario narrative and what it demonstrates
- **`PROMPT.md`** — a paste-ready prompt for your MCP-enabled agent
- **`expected/`** — captured output snapshots (quest list, lore catalog, brief) so you can see what guild produces without running anything
- **`setup.sh`** / **`teardown.sh`** — sandbox init + cleanup (creates a distinctly-named guild project so the example doesn't pollute your real board)

## How to engage

- **Skim** — read `README.md` + `expected/` to see what guild produces without touching it
- **Run it** — `./setup.sh`, paste `PROMPT.md` into your MCP client, `./teardown.sh` when done

> The `guild` CLI drives the same `~/.guild/*.db` state as the MCP tools — useful for scripting or CI. These examples use the agent-driven path because that's how users work day-to-day.

## Sandbox isolation

Each example initializes a distinctly-named project (e.g. `guild-example-01-hello-guild`) in your real `~/.guild/*.db`, contained to that project. Run `./teardown.sh` to clean up afterward.

> ⚠️ **Teardown is currently best-effort.** Clean single-project wipe is tracked at QUEST-152 (lore strike / quest strike) and QUEST-153 (guild raze). Until those land, teardown is a multi-step delete dance.
