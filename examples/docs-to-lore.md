# Docs to Lore

> *"I already have a bunch of design docs and runbooks. Can guild make them queryable?"*

You have a directory of Markdown notes: auth design, deploy runbook, data
model, ADRs. Right now they are readable but not queryable by an agent. You
want to turn that corpus into a project-specific retrieval layer so an agent
can find relevant context with `lore_appraise` before it starts writing code.

## What this example shows

- How to turn existing documentation into lore entries an agent can search
- The correct shape for a lore entry used as a doc pointer: distilled
  summary, keyword-rich tags, source path pointing back to the original file
- How the retrieval actually works under the hood (important: read before
  setting expectations)
- Common failure modes and how to fix them

This example is read-only (conceptual plus live output). It shows the
pattern, not a runnable scenario. For a runnable example with setup and
teardown, see [01-hello-guild](./01-hello-guild/).

## Primitives used

| Primitive | Role |
|---|---|
| `lore_inscribe` | Write one entry per doc; distilled summary + tags + source path |
| `lore_appraise` | Query the corpus by keyword; BM25 ranked results |
| `lore_study` | Fetch full entry detail (summary, tags, source path) |

## How retrieval works (read this first)

Guild's retrieval is SQLite FTS5 with BM25 ranking over three columns:
`title`, `summary`, and `tags`. This is defined in
`internal/storage/migrations/001_init.up.sql:67-70` and used in
`internal/lore/appraise.go:245`. There are no embeddings, no vectors, no
semantic similarity.

That means:

- **Lexical, not semantic.** "car" will not match "automobile". Keyword
  overlap between the query and the indexed fields is required to surface
  a result.
- **Three indexed fields.** Only `title`, `summary`, and `tags` are in the
  FTS index. The `source` field (the path back to your original doc) is
  stored on the entry but is not searchable. The body/rationale field on a
  lore entry is also outside the FTS index; it is retrievable via
  `lore_study` (progressive disclosure) but invisible to `lore_appraise`.
- **The searchable surface is the distilled metadata.** This is the whole
  trick: each doc's title, a 2-3 sentence summary written in the terminology
  users naturally query with, and a tag list that bridges vocabulary gaps.

One user who tried the pattern put it this way: "now it became a little RAG
system almost." The "almost" is doing real work. Guild is *lexical* RAG
(the same family as Elasticsearch or Lucene BM25 search), not *semantic*
RAG (not the same family as OpenAI-embeddings search or a vector database).
Queries that match on shared keywords retrieve well. Queries that rely on
concept synonymy do not, unless the tags bridge the gap.

The implication for how you inscribe entries is direct: tags are the
vocabulary bridge. If a user might query "login expiry" but the doc uses
"JWT refresh window", the entry needs both sets of terms in its tags.

## Walkthrough

Start with a directory of docs:

```
~/projects/myapp/docs/
  auth-design.md
  deploy-runbook.md
  data-model.md
```

### Option A: agent-inscribed (recommended)

Tell your agent to inscribe each doc with a distilled summary and rich tags.
The prompt that works well in practice:

```
For each Markdown file in docs/, inscribe a lore entry.
For each doc:
- Write a distinct 2-3 sentence summary in the terminology a developer would
  naturally use to query this topic. Do NOT paste the full doc body.
- Choose an appropriate kind (research for design docs, decision for ADRs,
  observation for runbooks).
- Add keyword-rich tags including synonyms a user might query with.
- Set source to the absolute file path.
- Set topic to a short slug matching the doc's domain.
```

What the agent produces for `auth-design.md`:

```
lore_appraise(query="authentication JWT token refresh")
# → nothing found

lore_inscribe(
  kind="research",
  title="auth-design: JWT token lifecycle",
  summary="JWTs expire at 1 hour; clients must refresh before expiry. Refresh window opens at 55 minutes to avoid race conditions. Refresh tokens rotate on each use and are stored in httpOnly cookies.",
  topic="auth",
  tags=["auth", "authentication", "jwt", "token", "refresh", "expiry",
        "login", "session", "cookie", "httponly"],
  source="~/projects/myapp/docs/auth-design.md"
)
```

Note: the summary is a distillation, not a paste of the doc. The tags
include synonyms (`authentication` alongside `auth`, `login` alongside
`session`) to bridge the vocabulary gap.

After inscribing all three docs:

```
$ guild lore list --project myapp

📜 3 entry(ies):
  LORE-1  [research · current]  auth-design: JWT token lifecycle
  LORE-2  [research · current]  data-model: core entities and soft-delete
  LORE-3  [research · current]  deploy-runbook: ECS rolling deploy and rollback
```

### Option B: guild lore catalog (fast, lower fidelity)

`guild lore catalog <dir>` bulk-imports Markdown files automatically.
Each file's title becomes the entry title; the file content becomes the
summary; the file stem becomes the topic.

```
$ guild lore catalog ~/projects/myapp/docs \
    --project myapp \
    --kind research

📚 cataloged: imported=3 skipped=0
```

This is fast but lower fidelity: the summary is the raw file body, not a
distillation. Tags are empty unless you pass `--tags`. Retrieval quality
degrades when summaries are long unstructured prose rather than
keyword-dense sentences. Start with catalog to bootstrap, then update
entries with `lore_update` (or re-inscribe with distilled summaries) as
you notice retrieval gaps.

## Live retrieval output

The following is captured from the live binary (guild v0.2.1) after
inscribing the three entries with the agent-inscribed approach above:

```
$ guild lore appraise "token refresh" --project myapp

🔮 1 entry(ies) appraised:

  LORE-1  [research · current · today]
  auth-design: JWT token lifecycle
  JWTs expire at 1 hour; clients must refresh before expiry. Refresh window
  opens at 55 minutes to avoid race conditions. Refresh tokens rotate on
  each use and are stored in httpOnly cookies.
  tags: auth,authentication,jwt,token,refresh,expiry,login,session,cookie,httponly
  source: ~/projects/myapp/docs/auth-design.md
```

```
$ guild lore appraise "deploy rollback" --project myapp

🔮 1 entry(ies) appraised:

  LORE-3  [research · current · today]
  deploy-runbook: ECS rolling deploy and rollback
  ...
```

**Vocabulary gap in action.** After importing with `catalog` (raw content,
no synonym tags), querying "login expiry" returns nothing even though the
auth doc covers exactly that:

```
$ guild lore appraise "login expiry" --project myapp
🔮 nothing found for "login expiry" — research needed
```

After re-inscribing with distilled summary and synonym tags (`login`,
`expiry`, `authentication`), the same query hits:

```
$ guild lore appraise "login expiry" --project myapp

🔮 1 entry(ies) appraised:

  LORE-1  [research · current · today]
  auth-design: JWT token lifecycle
  ...
  tags: auth,authentication,jwt,token,refresh,expiry,login,session,...
```

The tag `login` is what closed the gap. The summary alone did not contain
the word "login".

## Common failure modes

### 1. Pasting the full doc body into summary

The temptation is to put the whole Markdown file into `summary` so
"everything" is searchable. This has three problems.

First, it violates the 2-3 sentence principle. The `principle-too-long`
hint in the hints engine will fire and tell you.

Second, it does not make the whole body searchable in a useful way. FTS5
tokenizes and indexes the entire summary field, so a very long summary does
rank on its terms, but BM25 scoring degrades when the summary is full of
prose instead of keyword-dense sentences. Ranking quality drops and
unrelated results start surfacing.

Third, lore entries are the query layer, not the storage layer. The
original file is the canonical source. The entry's `source` field points
back to it. Use `lore_study` to retrieve the full file path and then read
the file. Pasting the body defeats this design and creates a maintenance
problem: now you have two copies of the content to keep in sync.

**Fix:** write a 2-3 sentence distillation. Store the file path in
`source`. Read the full content via the file when an agent needs it.

### 2. Expecting semantic similarity

If you query "vehicle performance" expecting to find docs about "car
latency benchmarks", you will be disappointed. There are no embeddings.
Guild does not know that "vehicle" and "car" are related. Retrieval is
exact token overlap after FTS5 normalization (lowercasing, stemming).

**Fix:** add synonym tags. If the doc uses "car" and users ask about
"vehicle", add both as tags: `["car", "vehicle", "automobile",
"benchmark", "performance", "latency"]`. This is the explicit vocabulary
bridge that replaces the implicit vocabulary bridge a semantic search
system would provide.

If your retrieval is consistently disappointing, inspect BM25 scores with
`--verbose` (planned for a future release; today: check `guild lore study
<ID>` to see the full entry and verify tags are set correctly).

### 3. Retrieval disappointing after catalog import

`catalog` imports produce empty tag fields and full-body summaries. Both
hurt retrieval quality. The quick fix is to update the high-value entries
with `lore_update` (add tags, shorten summary to 2-3 sentences) or
re-inscribe them properly and `lore seal` the imported versions.

### 4. Source paths become stale

If you move or rename the source doc, the `source` field on the lore entry
still points to the old path. Guild does not track file renames. Update the
entry with `lore_update` after a doc move, or re-inscribe.

## What an agent does with this

Once the corpus is inscribed, an agent working on a feature that touches
authentication would start by calling:

```
lore_appraise(query="auth JWT token", all_projects=True)
```

Guild returns the auth-design entry. The agent reads the summary, sees the
source path, and can read the full file if it needs implementation details.
The agent has project-specific context before writing a single line of code,
without requiring a human to provide it manually each session.

That is the pattern: docs stay in the source tree as the authoritative
reference; lore entries are the queryable index that makes them discoverable.

## See also

- [seeding-principles.md](./seeding-principles.md): a complementary
  pattern: inscribing behavioral conventions (not doc pointers) as
  principles that auto-load on every session
- [05-lore-only](./05-lore-only/): using guild purely as a knowledge layer
  without any quest ceremony
- [01-hello-guild](./01-hello-guild/): runnable cold-start example that
  shows the appraise-before-inscribe discipline
