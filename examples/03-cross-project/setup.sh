#!/usr/bin/env bash
# Initialize two sandbox projects for example 03-cross-project.
#   PROJECT_A — pre-seeded with a decision entry the agent will pull from
#   PROJECT_B — empty starting point; the agent will work here
set -euo pipefail

# 1. Version probe — fail fast if guild is not installed.
command -v guild >/dev/null || { echo "guild not installed — see https://github.com/mathomhaus/guild"; exit 1; }
echo "guild $(guild version)"

PROJECT_A="guild-example-03-project-a"
PROJECT_B="guild-example-03-project-b"
SANDBOX_A="/tmp/${PROJECT_A}"
SANDBOX_B="/tmp/${PROJECT_B}"

# 2. Create sandbox directories.
mkdir -p "${SANDBOX_A}" "${SANDBOX_B}"
echo "sandboxes: ${SANDBOX_A}, ${SANDBOX_B}"

# 3. git init — guild init requires a git repository (QUEST-108 tracks non-git mode).
git -C "${SANDBOX_A}" init -q
git -C "${SANDBOX_B}" init -q

# 4. Register both sandboxes as guild projects.
#    guild init auto-detects the project name from the directory basename.
(cd "${SANDBOX_A}" && guild init --yes)
(cd "${SANDBOX_B}" && guild init --yes)

# 5. Pre-seed project A with ONE decision entry.
#    This is the prior art the project-B agent will discover via lore_appraise(all_projects=true).
#    Title is a positional argument; --summary and --topic are required flags.
guild lore inscribe "Exponential backoff retry policy" \
  --project "${PROJECT_A}" \
  --kind decision \
  --summary "Starting at 100ms, max 3 retries — avoids thundering herd on flaky dependencies" \
  --topic networking

echo ""
echo "setup complete"
echo "  project A '${PROJECT_A}' — 1 decision entry seeded"
echo "  project B '${PROJECT_B}' — empty, ready for agent"
echo ""
echo "next: paste PROMPT.md into your MCP-enabled agent (working in project B context)"
