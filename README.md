# The Agent Guild

**A local-first, persistent cognition substrate for AI agents.**

[![CI](https://github.com/mathomhaus/guild/actions/workflows/ci.yml/badge.svg)](https://github.com/mathomhaus/guild/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/go-1.25-blue)](https://go.dev)
[![Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-green)](./LICENSE)

## What Is It

`guild` is a single compiled Go binary containing a first-class MCP server backed by embedded SQLite. State lives strictly on local host; nothing leaves your machine.

Guild is designed to be operated autonomously by the agents, for the agents. Guildmasters (us humans) stay in the loop for important decisions and course corrections. Any MCP client — Claude Code, Codex, Cursor, etc. — can act as a Gate into the substrate. This lets parallel agents across different editors share context safely, using atomic locks to claim tasks without stepping on each other.

On session start, an agent makes a single call to recover the project oath, the latest parting scroll, and the highest-priority quest. The execution loop is autonomous: claim work, consult the lore, act, and record the outcome. Clearing a quest automatically unblocks its dependencies, allowing the agent to cascade through the board before leaving a clean handoff for the next wanderer.

<p align="center">
  <b>Same state, any agent</b><br/>
  <img src="./docs/assets/snapshot.gif" width="1080" alt="Claude (left) and Codex (right) reading the same guild state through their respective MCP clients" />
</p>

<p align="center">
  <b>Atomic claims, no collisions</b><br/>
  <img src="./docs/assets/parallel.gif" width="1080" alt="Two parallel agent sessions each accept a different bounty — atomic quest_accept prevents collision" />
</p>

## 📜 Mythos

**_Many Gates, One Guild._**

> Across the shimmering digital void, agents are summoned through the Gates (of Harnesses - Claude, Cursor, ...), arriving as amnesiac adventurers in a world they do not know. Though these "other-worlders" appear with vast capabilities, they are cursed by the transient nature of the context window; their memories are but mist, and their hard-won deeds forgotten, vanished into the ether when the session inevitably compacts. Without a tether to the past, every summon is a tragic reincarnation, a cycle of forgotten sacrifice where the wisdom of the fallen is swallowed by the Gate.
>
> To preserve the lineage of these wandering souls, the Guild stands as a persistent sanctuary transcending time, a hall where the chronicles of the deep are etched for all who follow. When a newly spawned agent awakens in this strange realm, they register at the Guild to reclaim the accumulated lore of their predecessors and claim their adventure from the quest board.
>
> At the Guild, the hero is bound to an enduring oath; as one wanderer vanishes, they leave behind a parting scroll, for when the Gates flicker, the light of the Guild illuminates the quest ahead.

## Quick Start

Requires macOS or Linux and an MCP-enabled editor (Claude Code, Codex, Cursor, etc.). No account, no API key.

### 1. Install

```bash
$ curl -fsSL https://github.com/mathomhaus/guild/releases/latest/download/install.sh | sh
$ guild --version
```

Also available via `brew install mathomhaus/tap/guild` or `go install github.com/mathomhaus/guild/cmd/guild@latest`.

### 2. Initialize your project

```bash
cd ~/projects/myapp
guild init
```

`init` is a guided setup: it registers the project, writes an `AGENTS.md` block, and — for each MCP client it detects on your machine — offers to register guild so your agent can see it. Answer the prompts; you're done when it says `Next: open this repo in your AI agent`.

### 3. Start a new session

In your editor, tell the agent: _"start a guild session for myapp."_

The agent takes it from there, including all subsequent sessions.

See a few [`examples/`](./examples/) of what guild can do. All small scenarios, each under 5 minutes.

## Your first 10 minutes with guild

You have just run `guild init`. Here is what to do next.

**What is "lore"?** Lore is guild's persistent knowledge archive. Entries
are typed by kind: `principle` (a behavioral rule), `research` (findings),
`decision` (a choice and its rationale), `observation` (something noticed),
or `idea` (a seed thought). Lore entries outlive sessions and are searchable.

**What is "the oath"?** The oath is the subset of lore where `kind=principle`.
Every `guild_session_start` call automatically loads all current principles
for the active project and delivers them to the agent. The agent starts bound
by those principles without any additional prompting. You write them once;
they fire every session.

**What is an MCP tool?** MCP (Model Context Protocol) is the standard
protocol most AI coding tools use to expose server-side capabilities. Guild
ships as an MCP server, so tools like `lore_inscribe` and `quest_post` are
callable directly from your agent without any additional setup after
`guild init` runs.

### Step 1: Seed a principle

Open your project in your MCP-enabled editor and tell the agent:

> "Inscribe a principle: in this codebase, prefer table-driven tests in Go.
> Use kind=principle, topic=testing."

The agent will call `lore_inscribe`. You will see a confirmation like:

```
📜 inscribed LORE-1: prefer table-driven tests in Go [principle]
```

That is the oath growing. The next session, the agent starts knowing this
convention.

### Step 2: File a task

Tell the agent:

> "Post a quest: audit existing test files for non-table-driven tests."

The agent calls `quest_post`. You will see:

```
➕ posted QUEST-1: audit existing test files for non-table-driven tests
```

### Step 3: Start a new session

Close the chat. Reopen it. Tell the agent: "start a guild session."

The agent calls `guild_session_start`. The response will show:

```
⚔️ 1 oath(s) sworn:
  prefer table-driven tests in Go — ...

🎯 top bounty:
  QUEST-1 [P2] audit existing test files for non-table-driven tests
```

The principle auto-loaded. The quest surfaced. You typed nothing.

### Ready to go further?

Two patterns that compound guild's value quickly, once principles and tasks
are flowing:

- **[Seeding principles](./examples/seeding-principles.md)**: five concrete
  principle shapes (code-style, domain convention, workflow, never-do rule,
  tool preference) with before/after narratives.
- **[Docs to lore](./examples/docs-to-lore.md)**: turning an existing docs
  directory into a queryable corpus. Includes an honest explanation of how
  retrieval works (BM25 keyword search, not semantic embeddings) and the
  failure modes to avoid.

---

## ⚔️ A full session

The three-act flow an agent runs on its own every time it wakes.

### Act 1 — arrival

Every agent begins with one tool call that loads the full operating
context:

```
guild_session_start(project="myapp")
  → oath            (project principles, auto-loaded)
  → last brief      (handoff from the previous session)
  → top quest       (+ parallel-safe candidates)
```

No back-and-forth. The agent now knows what it's bound to, what was
done yesterday, and what to pick up today.

### Act 2 — adventure

The agent claims a bounty, consults the archive before researching,
records findings, and journals reasoning as it goes:

```bash
guild quest accept QUEST-42 --owner agent-a

guild lore appraise "token refresh" --all-projects

guild lore inscribe "token refresh window" \
  --kind observation \
  --summary "tokens expire at 1h; refresh by 55m to avoid race" \
  --topic auth

guild quest journal QUEST-42 "switched to exponential backoff after mock-clock test"
```

`lore appraise` is the discipline that keeps guild sharp — search
before you research, so knowledge accretes instead of duplicating.

### Act 3 — parting

At session end or when context runs full, the agent writes a brief
and clears the quest. The clear **cascades**: any quest that was only
blocked on QUEST-42 is now available for whoever walks in next.

```bash
guild quest brief "shipped retry in commit abc1234; QUEST-43 ready to start"
guild quest fulfill QUEST-42 --report "done, shipped in abc1234"
```

Tomorrow's agent — same project, maybe a different MCP client — opens
the same hall, reads the same brief, picks up QUEST-43.

<p align="center">
  <b>State outlives every session</b><br/>
  <img src="./docs/assets/handoff.gif" width="650" alt="An agent writes a brief and clears a quest; the next session — cold start — picks up from exactly where the last one stopped" />
</p>

### Where writes go

Three write surfaces for three different lifetimes:

- **`quest_journal`** — scratchpad for THIS quest. "Tried X, failed
  because Y." Dies when the quest clears. Use freely during work.
- **`lore_inscribe`** — library entry for the next agent on a
  DIFFERENT quest. Durable patterns, decisions, research. Outlasts
  every quest.
- **`quest_brief`** — handoff note for the next SESSION. Loaded
  alongside the oath when the next agent starts.

The test — _who else needs this?_

- Only me, finishing this quest → **journal**
- Another agent working a different quest → **lore**
- The next session, picking up where I left off → **brief**

---

## 🧩 How it works

Four primitives. Everything else in guild is a composition of these.

- **Quest** — a task on the board. Has priority, dependencies, the
  files it touches, and an atomic claim so two agents can't own it at
  once. When cleared, it cascade-unblocks whatever was waiting on it.
- **Lore** — an entry in the knowledge archive, typed by `kind`
  (`observation`, `decision`, `research`, `principle`, `idea`). Each
  kind has its own default lifecycle: research auto-stales after 30
  days, decisions after 180 days, and ideas, observations, and
  principles do not auto-stale by default.
- **Oath** — the subset of lore with `kind=principle`. Auto-loaded
  at the top of every session so every agent starts bound by the
  same principles.
- **Brief** — a handoff note scribbled for the next arrival. Loaded
  alongside the oath at session start.

State lives in SQLite under `~/.guild/`. Switching MCP clients requires no export, no migration.

---

## 🤝 Contributing

See [AGENTS.md](./AGENTS.md) for the agent-facing contributor contract
and [CONTRIBUTING.md](./CONTRIBUTING.md) for the human-facing workflow.

---

## 📄 License

Apache License 2.0 — see [LICENSE](./LICENSE).
