# Seeding Principles

> *"I keep correcting the same agent mistakes. How do I make them stick?"*

You have a project convention the team follows religiously: table-driven
tests in Go, a specific AWS auth helper, no em dashes in generated text,
and a domain architecture rule. You are tired of restating these every
session. Inscribe them as principles once and every future session loads
them automatically, without a prompt.

## What this example shows

- How to inscribe project-specific conventions as `kind=principle` lore entries
- How the oath wall (the auto-loaded set of principles) shapes agent behavior
  from session start, without repeating yourself
- Five principle shapes: code-style, domain convention, workflow preference,
  never-do rule, and tool preference
- The 60-word discipline: principles longer than 60 words belong in a
  `kind=decision` entry; the principle just points there

This example is read-only (conceptual). It shows the pattern, not a
runnable scenario. For a runnable end-to-end, see
[01-hello-guild](./01-hello-guild/).

## Primitives used

| Primitive | Role |
|---|---|
| `lore_inscribe(kind=principle)` | Write a rule that auto-loads every future session |
| `lore_appraise` | Check for duplicates before inscribing |
| `guild_session_start` | Loads the oath wall; all principles surface here |

## How principles become oath

When you call `guild_session_start`, guild loads every `kind=principle`
entry (status=current) for the active project and delivers them to the
agent before any tool call. The agent starts bound by them. This is the
oath.

There is nothing else to configure. Inscribe a principle once; it fires
on every future session for that project.

## The 60-word bar

A principle auto-loads every session. That has a cost: every token in
the principle is paid by every future agent, forever. So the bar is
tight: if a principle needs more than 60 words to express, it's a
decision in disguise. Inscribe the rationale as `kind=decision`, then
write a short principle that references it.

## Example 1: code-style

**Convention:** Go tests use table-driven style.

```
lore_appraise(query="Go testing style")
# → nothing found

lore_inscribe(
  kind="principle",
  title="prefer table-driven tests in Go",
  summary="Go tests use table-driven style: define a slice of structs (name, input, want), range over them, call t.Run. Subtests are named. Avoids test-per-variant function sprawl.",
  topic="testing",
  tags=["go", "testing", "table-driven", "code-style"]
)
```

**Without the principle:** an agent writes one test function per case,
named `TestFooReturnsBarOnInput1`, `TestFooReturnsBarOnInput2`. Valid Go,
but not the project convention.

**With the principle:** the agent writes a single `TestFoo` function with
a `cases` slice and a `for _, tc := range cases { t.Run(tc.name, ...) }`
loop. No prompt needed.

What the oath shows next session:
```
⚔️ 1 oath(s):
  prefer table-driven tests in Go — Go tests use table-driven style:
  define a slice of structs (name, input, want), range over them, call
  t.Run. Subtests are named. Avoids test-per-variant function sprawl.
```

## Example 2: domain convention

**Convention:** this codebase follows hexagonal architecture; the domain
layer must stay free of infrastructure imports.

```
lore_inscribe(
  kind="principle",
  title="hexagonal architecture: keep domain layer free of infra imports",
  summary="The domain package (internal/domain) must not import infra packages (internal/storage, internal/http, external SDKs). Adapters live in internal/adapters. Violations break the dependency inversion that makes the domain testable without mocks.",
  topic="architecture",
  tags=["hexagonal", "domain", "architecture", "infra", "dependency-inversion"]
)
```

**Without the principle:** an agent adds a database call directly inside a
domain type to satisfy a feature request. It compiles. It breaks the
architecture contract.

**With the principle:** the agent routes the change through an adapter,
keeps the domain pure, and adds a port interface if none exists. The
convention holds without a code-review reminder.

## Example 3: workflow preference

**Convention:** always run `make check` before calling a task done.

```
lore_inscribe(
  kind="principle",
  title="run make check before calling a task done",
  summary="Always run make check (fmt + vet + lint + sqlcheck + test-race) before marking any quest fulfilled. A green CI gate is the definition of done for this repo.",
  topic="workflow",
  tags=["workflow", "make", "check", "done-criteria", "ci"]
)
```

**Without the principle:** an agent fulfills a quest after the unit tests
pass locally, but the linter catches a format issue in CI and the PR fails.

**With the principle:** the agent runs `make check` before writing the
fulfillment report and catches the issue before the PR is opened.

## Example 4: never-do rule

**Convention:** no em dashes in any generated artifact.

```
lore_inscribe(
  kind="principle",
  title="no em dashes in generated text",
  summary="Never output the em dash character in any artifact: commits, PR bodies, code comments, docs. Use a period, comma, parentheses, or colon instead. Applies to every tool call in every session.",
  topic="style",
  tags=["style", "formatting", "em-dash", "never-do", "punctuation"]
)
```

**Without the principle:** an agent writes "we shipped the feature; it is
now in production" with an em dash. The commit passes review but violates
the house rule.

**With the principle:** the agent uses a semicolon or splits into two
sentences. The rule fires from the oath wall before the agent writes a
single word.

This particular principle is self-dogfooding: the guild repo itself uses
it (see `examples/01-hello-guild/` for the original scenario it came from).

## Example 5: tool preference

**Convention:** use `keyconjurer` for AWS credential vending, not
`aws configure` or hardcoded keys.

```
lore_inscribe(
  kind="principle",
  title="use keyconjurer for AWS auth, not aws configure",
  summary="Obtain AWS credentials via keyconjurer (keyconjurer get <role>), not aws configure or hardcoded keys. Keyconjurer enforces SSO and short-lived credentials. Storing long-lived keys in ~/.aws/credentials is a security violation on this team.",
  topic="aws",
  tags=["aws", "auth", "keyconjurer", "credentials", "security", "sso", "tool-preference"]
)
```

**Without the principle:** an agent scaffolds an AWS SDK integration and
adds `aws configure` instructions to the setup docs, or worse, suggests
hardcoding an access key for local testing.

**With the principle:** the agent generates a `keyconjurer get
<role>` step in the setup instructions and warns against long-lived
credential storage. No human follow-up needed.

## Live output reference

The following is captured from the live binary (guild v0.2.1). After
inscribing the three principles from above:

```
$ guild lore oath --project myapp

⚔️ 3 oath(s):
  no em dashes in generated text — Never output the em dash character (—)
  in any artifact: commits, PR bodies, code comments, docs. Use a period,
  comma, parentheses, or colon instead. Applies to every tool call in
  every session.

  prefer table-driven tests in Go — Go tests use table-driven style:
  define a slice of structs (name, input, want), range over them, call
  t.Run. Subtests are named. Avoids test-per-variant function sprawl.

  run make check before calling a task done — Always run make check
  (fmt + vet + lint + sqlcheck + test-race) before marking any quest
  fulfilled. A green CI gate is the definition of done for this repo.
```

The same three lines appear in `guild_session_start` output under
`oath(s) sworn` when a session opens on this project.

## See also

- [docs-to-lore.md](./docs-to-lore.md): turning existing documentation
  into a queryable lore corpus
- [01-hello-guild](./01-hello-guild/): runnable cold-start example that
  inscribes a principle and shows the oath feedback loop
- [04-session-handoff](./04-session-handoff/): how principles and briefs
  compound across sessions
