# Prompts

This example has **two prompts** — one per session. Run them in order with a **real context switch** between them.

---

## Session A — do the partial work, leave a brief

After running `./setup.sh`, paste the following into your MCP-enabled agent (Claude Code, Codex, Cursor, etc.):

```
I'm mid-rename: User.email → User.primaryEmail, project is
guild-example-04-session-handoff. I've made it through the models/
layer and updated the four handler call sites, but services/ and
test fixtures still use the old name.

I need to stop now and hand off. First, pin the naming convention
I'm establishing so it's durable (something like "primary-field
prefix for email fields"). Then leave a substantive brief for the
next agent — what's done, what's next, and the gotcha that
fixtures/users_test.go still references the old name and will cause
false failures if not updated before running services/ tests.
```

---

┌─────────────────────────────────────────────────────────────────────────────────┐
│  STOP — open a NEW agent context before pasting Session B.                      │
│                                                                                 │
│  Close this chat (or open a new one) before continuing. If you want to feel     │
│  the full effect, switch MCP clients entirely — e.g. finish Session A in        │
│  Claude Code, then start Session B in Cursor or Codex. The brief lives in       │
│  ~/.guild/*.db and is client-agnostic.                                          │
│                                                                                 │
│  Running Session B in the same conversation still works mechanically, but you   │
│  will not experience the context-reset that makes the handoff real.             │
└─────────────────────────────────────────────────────────────────────────────────┘

---

## Session B — pick up where A left off

In a **fresh agent context** (new chat, or a different MCP client), paste:

```
I'm continuing work in project guild-example-04-session-handoff but
have no memory of the previous session. Please check for any
handoff notes left for me, then pick up where the previous agent
stopped.

When you're done with the next chunk, record what you completed —
acknowledge the gotcha from the brief if it's still open — so the
trail stays clear.
```

That's the whole prompt for each session — no function signatures,
no step-by-step tool calls. Session A's "leave a brief" cue tells the
agent to call `quest_brief`; Session B's `session_start` automatically
surfaces that brief at the top of its output, so "check for handoff
notes" resolves into reading what's already there.

Expected: Session A writes one observation + a substantive brief.
Session B's `session_start` surfaces A's brief, the agent continues
the rename into services/, and records a follow-up observation. See
[expected/](./expected/) for captured snapshots from both sides.
