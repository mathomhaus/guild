# Expected output

Captured snapshots from a reference run of this example. Both sessions were simulated
sequentially in one agent context against a fresh `guild-example-04-session-handoff`
project. The DB state transitions are what matter; the LORE IDs in these files will
differ from your run if other entries were inscribed beforehand.

## Files

- `session-a-brief.txt` — the brief text written at the end of Session A, captured
  via `guild status --brief` from inside the sandbox directory.
- `session-b-start.txt` — the full `guild_session_start` / `guild status` output at
  the top of Session B, showing A's brief surfacing automatically.
- `lore-catalog-final.txt` — output of `guild lore list` after both sessions complete,
  showing both inscribed observations.

## What to look for

In `session-b-start.txt`, the brief appears **before** any oath or quest output. That
is the anti-amnesia mechanic: the incoming agent sees the handoff context as the very
first thing, not buried at the bottom.

In `lore-catalog-final.txt`, both entries share the same `refactor-convention` topic,
making it easy to retrieve the full rename history with a single `lore_appraise` call.
