# Prompt

After running `./setup.sh`, paste the following into your MCP-enabled agent (Claude Code, Codex, or Cursor — anything with Agent-tool access for parallel subagents):

```
I want to plan a rate limiter for an HTTP API. The project
guild-example-02-quest-decomposition already has the work split
into four pieces on the task board: algorithm choice, storage
backend, pitfalls to avoid, and an observability signal.

Grab the parent task, then split the four pieces across four
subagents so they work in parallel — one per piece. Each subagent
should pick up its piece, decide on one concrete answer, save that
choice with reasoning, and close its task when done.

Once all four wrap up, combine their picks into one integrated
design for the rate limiter, save that as the design we're going
with, close out the parent task, and leave a handoff note for
whoever implements this next.
```

That's the whole prompt — no function signatures, no per-subagent
step lists, no kind labels. The "split across four subagents so
they work in parallel" cue tells the parent to use its Agent-tool
for a single-message parallel spawn; atomic `quest_accept` handles
collision prevention without explicit coordination.

Commitment phrasing carries the classification signal: *"decide on
one concrete answer"* and *"save that as the design we're going
with"* both land as `kind=decision` without needing the label.

Expected: all 5 quests fulfilled, 5 new lore entries inscribed —
4 `decision` (algorithm, storage, observability + parent synthesis)
and 1 `research` (pitfalls list; list-shape content naturally lands
as findings, not a single commitment). Plus one closing brief. See
[expected/](./expected/) for captured snapshots from a reference
run.
