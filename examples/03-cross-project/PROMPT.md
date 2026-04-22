# Prompt

After running `./setup.sh` (which seeds two sandbox projects — A with pre-existing lore, B empty), paste the following into your MCP-enabled agent. The agent should be working in project B's context (`guild-example-03-project-b`).

```
I'm starting the project guild-example-03-project-b. Before I decide
a retry policy for this service, check whether I've already made
this kind of decision in another project I've worked on — if so,
I'd rather inherit it than re-derive it.

If you find prior art, read the full entry, then save my decision
here in B with a clear link back to the source.
```

That's the whole prompt — guild's cross-project appraise and the
`--informs` provenance edge are enough for the agent to build the
citation graph itself. You don't need to name specific tools.

Expected: the agent surfaces project A's exponential-backoff decision
via `lore_appraise(all_projects=true)`, studies it, then inscribes a
decision in project B with an `informs` edge pointing back. See
[expected/informs-edge.txt](./expected/informs-edge.txt) for the
resulting cross-project linkage.
