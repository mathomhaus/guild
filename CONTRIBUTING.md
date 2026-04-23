# Contributing to guild

Thanks for being here. guild is a small, opinionated project — the
contributor bar is "does this keep the guild agent-agnostic,
local-first, and disciplined." If you're unsure whether something fits,
open a Discussion before writing code.

## Before you start

- Read [`AGENTS.md`](./AGENTS.md) (repo contributor conventions) and
  [`README.md`](./README.md) (end-user perspective).
- Skim [`internal/mcp/instructions.go`](./internal/mcp/instructions.go) —
  this is the full operating contract the MCP server ships to clients.

## Fork workflow (for external contributors)

If you don't have push access to `mathomhaus/guild` — the default for
anyone outside the core maintainer set — fork first, then work on a
branch in your fork and open a PR against `mathomhaus/guild:main`.

```bash
gh repo fork mathomhaus/guild --clone
cd guild
git checkout -b short-descriptive-branch-name
# ... make changes, commit ...
git push origin short-descriptive-branch-name
gh pr create --repo mathomhaus/guild
```

Or fork via the GitHub UI, then clone your fork locally. Either works.

### Claiming an issue

Comment `/assign` on an issue labeled `good first issue` or `help
wanted` to claim it; use `/unassign` to release it if you need to drop
it. (Until the assign bot is live, just say so in a comment — a
maintainer will assign you.)

Being assigned means the issue is yours to work on; it does **not**
grant push access to this repo. You still fork and open a PR from
your fork.

## Development setup

This path is for contributors with push access (maintainers,
collaborators). External contributors should use the
[fork workflow](#fork-workflow-for-external-contributors) above
instead.

```bash
git clone https://github.com/mathomhaus/guild.git
cd guild
make check        # fmt + vet + lint + sqlcheck + test-race — the gate
```

Common commands:

```bash
make help              # list every make target
make check             # the full pre-commit gate
make test-race         # just the race-enabled tests
make install           # build and install ./cmd/guild to $GOBIN
make release-snapshot  # goreleaser dry-run (no publish)
```

Go 1.25+ is required. No CGO — SQLite is provided by the pure-Go
`modernc.org/sqlite`.

## Using guild while working on guild

Dogfooding is the point. Run `guild mcp install` once, then let your
agent pick up quests from the live board:

```
mcp__guild__guild_session_start(project="guild")
```

The server's `INSTRUCTIONS` string enforces the tool discipline (always
`lore_appraise` before researching, narrate after mutations, pick the
right kind for each inscribe). Follow it.

## Commit style

- Short, imperative subject lines. Prefix with the usual conventional
  verbs when useful: `feat:`, `fix:`, `chore:`, `docs:`, `refactor:`,
  `test:`.
- Scope the message to behavior, not implementation. "why," not "what."
- If the change closes a quest on the internal board, append
  `(QUEST-N)` to the subject.
- **No AI attribution.** Do not add `Co-Authored-By: Claude` trailers,
  `🤖 Generated with ...` lines, or AI-authored comments. Tools are
  not co-authors.

## Pull requests

- One logical change per PR.
- `make check` must pass locally before you open the PR. CI runs the
  same gate.
- Update or add tests for behavior changes. A test that only re-states
  the implementation is not a test — exercise the user-visible contract.
- Update `CHANGELOG.md` under `## [Unreleased]` if your change is
  user-visible.

## Adding dependencies

Don't, unless essential. Every new module is a supply-chain surface and
a binary-size hit. Prefer stdlib, then a minimal in-tree implementation,
then — last — a well-audited third-party package.

## Reporting issues

Use the GitHub issue forms. For security, use
[GitHub Security Advisories](./SECURITY.md) — not public issues.
