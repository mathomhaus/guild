# Campaigns

Quests can be grouped into a campaign. A campaign is a free-text label that
collects related quests so they can be reported on as a unit. The same field
is also accepted as `epic` for backward compatibility, but `campaign` is the
canonical spelling. When both are set at the boundary, `campaign` wins.

The active set of campaign names lives in your local quest database. There is
no central registry in this repo, by design — campaigns are local to each
project's quests.

## Why this doc exists

The list of in-use campaigns has grown organically across projects. Without a
shared mental model, the next person filing a quest tends to invent a new
near-duplicate (`mcp` vs `mcp-contract` vs `mcp-autonomy`, `launch` vs
`launch-promo`) instead of reusing what already exists. This page describes
how to think about campaigns so that next quest filer makes a deterministic
choice.

## Listing the active campaigns

`quest guild` is the canonical command. It prints a per-project summary
grouped by campaign, with counts of next, in-progress, blocked, and done
quests:

```bash
guild quest guild
```

Run it before filing a new quest. If a campaign that already covers the work
shows up, reuse it. The MCP equivalent is the `quest_guild` tool — same
output, same grouping.

There is no static list of campaigns to memorise. The live list is whatever
`quest guild` prints in your project today.

## When to reuse vs create a new campaign

Reuse an existing campaign when:

- The quest is the next step of a piece of work already grouped under that
  campaign.
- The quest's outcome will be reported alongside the other quests in that
  campaign (e.g. "MCP autonomy progress" rolls up cleanly).
- A near-synonym campaign already exists and creating a new one would
  produce a duplicate (`mcp` and `mcp-contract` both already cover MCP
  surface work — pick one and reuse).

Create a new campaign when:

- The work is a new initiative with a distinct outcome that deserves its own
  rollup.
- No existing campaign in `quest guild` cleanly covers the work — and the
  near-duplicates check above doesn't apply.
- The work spans multiple existing campaigns and rolling them up under a
  single new campaign would meaningfully clarify reporting.

If you would have to explain the campaign in two sentences, it's probably
two campaigns. If you're not sure, reuse — `quest campaign` (the rename
verb) exists and is cheap to apply later.

## Setting and changing the campaign on a quest

Set the campaign at quest creation time, or apply it to a batch of existing
quests with the rename verb:

```bash
guild quest campaign <campaign-name> QUEST-1 QUEST-2 ...
# `quest epic` works as an alias for the same operation.
```

`quest campaign` is the canonical CLI verb; `quest epic` is registered as
an alias. The underlying field is `campaign` and the MCP tool name stays
`quest_epic` for backward compatibility, with `--campaign` and `--epic`
both accepted as input.

## Out of scope

Renaming or merging existing campaigns is intentionally out of scope of this
doc. If your `quest guild` output shows near-duplicates that should collapse,
file a quest for the merge work itself rather than treating it as a docs
update.

There is also no `guild campaign` verb to manage the campaign list
programmatically. Filings happen via `quest campaign` on the affected
quests.
