# Changelog

All notable changes to guild are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project will adhere to [Semantic Versioning](https://semver.org/) once
it reaches 1.0.

## [Unreleased]

## [0.3.1] - 2026-04-24

### Changed

- Pre-built binaries (install.sh, Homebrew tap) now include semantic retrieval
  assets. Users installing via `curl | sh` or `brew install` get vector search
  working on first run with no extra steps.
- `make install` now defaults to `-tags=withembed` (ship-ready). Run
  `make install-fast` for the faster no-embed dev path.
- `make build` likewise defaults to `-tags=withembed`. `make build-fast` is
  the no-embed alternative.
- `make install-embed` and `make build-embed` are kept as no-op aliases for
  one release cycle so existing scripts do not break; both targets print a
  deprecation note.
- goreleaser config (`-tags=withembed` in build flags) ensures GitHub release
  artifacts ship the full-embed binary.
- README install section updated: three paths documented (install.sh/brew for
  ship-ready embed; `make install` for clone-and-build embed; `go install`
  explicitly noted as keyword-only).

[Unreleased]: https://github.com/mathomhaus/guild/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/mathomhaus/guild/compare/v0.1...v0.3.1

## [0.1] - 2026-04-17

### Added

- Initial public release of `guild`: a local-first, agent-agnostic MCP
  server plus CLI for persistent quest management and knowledge (lore)
  lifecycle across AI-agent sessions.
- `guild lore` verbs: `inscribe`, `appraise`, `study`, `reforge`,
  `commune`, `dossier`, `link`, `meld`, `oath`, `list`, `inquest`,
  `update`.
- `guild quest` verbs: `post`, `accept`, `clear`, `list`, `brief`,
  `journal`, `pulse`, `scroll`.
- `guild mcp serve` — stdio MCP server exposing all lore and quest
  tools to any MCP-speaking client (Claude Code, Codex, Cursor,
  Windsurf, Zed, VS Code extensions, …).
- `guild mcp install` — prints (or, with `--run`, executes) the
  registration command for each detected MCP client.
- `guild init` — prints the recommended `AGENTS.md` content and
  per-client registration commands; writes with `--write` or `--merge`.
- `guild migrate` — one-shot importer for the pre-Go Python prototype's
  `~/.lore/` and `~/.quest/` trees.
- Pure-Go SQLite (`modernc.org/sqlite`) storage at `~/.guild/lore.db`
  and `~/.guild/quest.db`. Schema self-migrates on first touch.
- Cross-project lore dedup by default on `lore_inscribe`; explicit
  `strict_project=true` to opt out.
- Oath system — principles auto-loaded at `guild_session_start`.
- Narration-as-UX — state-changing tools emit emoji-prefixed lines so
  MCP clients that collapse tool output still surface effects.
- Single static binary built with `CGO_ENABLED=0` for macOS (amd64 +
  arm64) and Linux (amd64 + arm64). goreleaser config included.

### Security

- No outbound network calls in any code path. All state lives under
  `~/.guild/` on the user's machine.

[0.1]: https://github.com/mathomhaus/guild/releases/tag/v0.1
