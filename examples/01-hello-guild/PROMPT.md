# Prompt

After running `./setup.sh`, paste the following into your MCP-enabled agent (Claude Code, Codex, Cursor, etc.):

```
I just installed guild on this project (guild-example-01-hello-guild).
Two things to set up:

First, a house rule that every future session picks up automatically:
no em dashes in generated text — use hyphens or rephrase. They read
as AI-generated.

Second, I'll need to audit existing docs for em-dash violations. Put
that on the task board as something to work on later.

Save both, then start a fresh session and show me the oath loaded
and the audit task waiting.
```

That's the whole prompt — no function signatures, no step-by-step
tool calls. guild's MCP tools and `session_start` conventions give
your agent everything it needs to translate intent into the right
verbs (`lore_inscribe` with `kind=principle` for the rule, `quest_post`
for the audit task, then `guild_session_start` to surface both).

Expected: the agent inscribes the principle + posts the quest, then
runs `session_start` and reports that the oath now shows the
no-em-dashes rule AND a top bounty (the audit task) is waiting. See
[expected/session-start-output.txt](./expected/session-start-output.txt).
