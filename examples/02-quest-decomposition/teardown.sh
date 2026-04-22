#!/usr/bin/env bash
# Clean up the sandbox project for example 02-quest-decomposition.
#
# NOTE: clean single-project teardown is currently best-effort.
#       Full implementation is blocked on:
#         QUEST-152 — guild lore strike (bulk-delete lore entries for a project)
#         QUEST-153 — guild raze (full project teardown verb)
#       Until those land, this script does a multi-step best-effort delete:
#       it iterates all open quests and fulfills/clears them, then removes
#       the sandbox dir. Lore entries and project registration persist in
#       ~/.guild/*.db until QUEST-152/153 are resolved.
set -euo pipefail

PROJECT="guild-example-02-quest-decomposition"

echo "Tearing down sandbox project: $PROJECT"

# Best-effort: fulfill any remaining open quests so the board is clean
OPEN=$(guild quest list -p "$PROJECT" --json --no-emoji 2>/dev/null \
  | python3 -c "import sys,json; qs=json.load(sys.stdin); [print(q['id']) for q in qs if q.get('status') not in ('fulfilled','done')]" \
  2>/dev/null || true)

if [ -n "$OPEN" ]; then
  echo "Clearing open quests: $OPEN"
  for QID in $OPEN; do
    guild quest fulfill "$QID" -p "$PROJECT" \
      --report "teardown: sandbox cleanup" --no-emoji 2>/dev/null || true
  done
fi

# Remove the sandbox temp dir if it still exists from setup.sh
# (setup.sh uses a temp dir under /tmp; nothing to remove here)
echo ""
echo "Teardown complete (best-effort)."
echo "Note: lore entries and project registration remain in ~/.guild/*.db"
echo "      until QUEST-152 (lore strike) and QUEST-153 (guild raze) land."
