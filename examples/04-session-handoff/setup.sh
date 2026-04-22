#!/usr/bin/env bash
# Initialize the sandbox project for example 04-session-handoff.
set -euo pipefail

PROJECT="guild-example-04-session-handoff"
SANDBOX="/tmp/${PROJECT}"

# 1. Version probe — fail fast if guild is not installed.
command -v guild >/dev/null || { echo "guild not installed — see https://github.com/mathomhaus/guild"; exit 1; }
echo "guild $(guild version)"

# 2. Create sandbox directory.
mkdir -p "${SANDBOX}"
echo "sandbox: ${SANDBOX}"

# 3. git init — guild init requires a git repository.
git -C "${SANDBOX}" init -q

# 4. Register the sandbox as a guild project.
# guild init auto-detects the project name from the directory basename.
(cd "${SANDBOX}" && guild init --yes)

echo "setup complete — project '${PROJECT}' registered"
echo "Next: paste Session A from PROMPT.md into your MCP-enabled agent."
