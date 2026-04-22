# 04 — session-handoff

> *"I'm stopping for the day; how do I hand off to tomorrow-me (or another agent)?"*

You are mid-flight on a rename refactor: `User.email → User.primaryEmail`. You have
made it through the model layer, but services and fixtures are still untouched. You
need to stop — context window, end of day, shift change — and hand off cleanly to the
next agent that picks this up, whether that is you tomorrow morning or a parallel
instance running in a different MCP client.

Guild's answer is `quest_brief`. At the end of Session A the agent writes a
substantive brief covering what was done, what is next, and the gotcha to watch for.
When Session B opens, `guild_session_start` surfaces that brief at the top of its
response — before the oath, before the quest list — so the incoming agent has full
context before touching a single file.

This example walks through both halves. Session A does partial work and leaves a
brief. Session B starts fresh, reads the brief automatically, and continues from
exactly where A stopped.

## What this example shows

- **`quest_brief`** — captures session-end handoff context in guild's persistent store.
  Survives context compaction, agent restart, and MCP-client switches.
- **`guild_session_start` surfaces briefs automatically** — the next session's first
  call returns the most recent brief at the top, with no manual retrieval needed.
- **`lore_inscribe` (kind=observation)** — pins a durable convention decision so the
  brief can reference it by ID rather than re-state it.
- **Agent-agnostic persistence** — the brief lives in `~/.guild/*.db`, which every
  MCP client reads from. Switch from Claude Code to Cursor and the brief is still
  there.

## How to run

### Agent-driven (required)

This example **requires two separate agent contexts**. There is no `run.sh`.

1. Run `./setup.sh` to initialise the sandbox project.
2. Open your MCP client and paste **Session A** from [PROMPT.md](./PROMPT.md).
3. **Open a new chat (or switch to a different MCP client entirely).**
   Do not continue in the same conversation.
4. Paste **Session B** from [PROMPT.md](./PROMPT.md).
5. Run `./teardown.sh` when done.

### Why there is no run.sh

The point of 04 is that the brief survives a real context reset — the moment when an agent
forgets everything and has to re-orient. Only a real context switch (new chat, new MCP-client
session) creates that reset. A shell script would silently skip it.

Running both prompts back-to-back in the same agent context still works mechanically — guild
stores and retrieves the brief correctly — but you would not feel the effect the way a cold
reader does.

## What to expect

**Session A** starts with `session_start` (empty brief — project is new), inscribes
one small observation about the `primaryEmail` naming convention, then calls
`quest_brief` with a ~70-word handoff covering what was done, what is next, and the
test-fixture gotcha.

**Session B** starts with `session_start`. This time the response leads with A's brief
before anything else. The agent reads it, acknowledges the context, and inscribes a
follow-up observation marking the services layer done and flagging the still-open
fixtures work.

After both sessions, `guild lore list` shows two entries: the convention observation
from A and the continuation observation from B.

## Primitives used

| Primitive | Session | Purpose |
|-----------|---------|---------|
| `guild_session_start` | A | Bootstraps session; returns empty brief (new project) |
| `lore_inscribe` (kind=observation) | A | Pins primary-field naming convention |
| `quest_brief` | A | Writes the session-end handoff |
| `guild_session_start` | B | **Returns A's brief at the top** |
| `lore_inscribe` (kind=observation) | B | Marks services/ done; flags fixtures |

## See also

- Prerequisite: [01-hello-guild](../01-hello-guild/) for primitive basics
- Grand-tour next step: [03-cross-project](../03-cross-project/) — handoff across projects, not just time
