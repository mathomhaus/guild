#!/usr/bin/env bash
# Initialize the sandbox project for example 02-quest-decomposition.
# Creates the project and pre-seeds 1 parent quest + 4 sub-quests.
# Run once before pasting PROMPT.md into your agent.
set -euo pipefail

PROJECT="guild-example-02-quest-decomposition"

# Register the sandbox project. guild init requires a git repo at cwd.
# We use a temp dir so this script is runnable from anywhere.
SANDBOX_DIR="$(mktemp -d)"
trap 'rm -rf "$SANDBOX_DIR"' EXIT

cd "$SANDBOX_DIR"
git init -q
guild init --yes --no-emoji 2>/dev/null | grep -E "registered|error" || true

echo "Sandbox project: $PROJECT"

# Post parent quest
PARENT=$(guild quest post "Plan a rate limiter" \
  -p "$PROJECT" \
  --priority P2 \
  --acceptance "sub-quests accepted and fulfilled by parallel subagents" \
  --acceptance "parent synthesizes findings into one kind=decision lore entry" \
  --acceptance "parent briefs closing" \
  --no-emoji 2>/dev/null)
echo "$PARENT"

# Post 4 sub-quests (independent — no depends-on so subagents can accept in parallel)
guild quest post "Recommend ONE rate-limiting algorithm, one-sentence rationale" \
  -p "$PROJECT" --priority P2 --no-emoji 2>/dev/null

guild quest post "Recommend ONE storage backend for counters, one-sentence rationale" \
  -p "$PROJECT" --priority P2 --no-emoji 2>/dev/null

guild quest post "List 3 pitfalls to avoid, one line each" \
  -p "$PROJECT" --priority P2 --no-emoji 2>/dev/null

guild quest post "Recommend ONE observability signal to emit, one-sentence rationale" \
  -p "$PROJECT" --priority P2 --no-emoji 2>/dev/null

echo ""
echo "Setup complete. Project: $PROJECT"
echo "Quests seeded:"
guild quest list -p "$PROJECT" --show-blocked --no-emoji 2>/dev/null || true
echo ""
echo "Next: paste PROMPT.md into your MCP-enabled agent (Claude Code, Codex, or Cursor)."
