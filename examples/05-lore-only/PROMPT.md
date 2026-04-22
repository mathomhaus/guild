# Prompt

After running `./setup.sh`, paste the following into your MCP-enabled agent (Claude Code, Codex, Cursor, etc.):

```
Some CI findings I want to save, project guild-example-05-lore-only,
under topic "ci":

- 7 min builds total, npm install eats 60% of that without any cache
- Going to add an npm ci cache keyed on package-lock.json — should
  save ~3 min
- Long-term, bun might be worth looking at — would replace the cache
  approach entirely, but not urgent

Save each separately and show me all three when done.
```

That's the whole prompt.

The content itself carries the classification signal: a factual
measurement → `observation`; _"going to add X"_ (committed action) →
`decision`; _"long-term... not urgent"_ (speculative) → `idea`. The
final _"show me all three"_ triggers a `lore_appraise` on the shared
topic.

Expected: 3 lore entries inscribed (one per kind, all `topic=ci`),
then a final appraise returning them clustered together. See
[expected/](./expected/) for captured snapshots.
