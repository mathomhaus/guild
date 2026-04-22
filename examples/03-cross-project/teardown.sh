#!/usr/bin/env bash
# Clean up both sandbox projects for example 03-cross-project.
#
# NOTE: sandbox directory removal is best-effort (set +e).
#       Project registration in ~/.guild/*.db persists until QUEST-152 (lore/quest strike)
#       and QUEST-153 (guild raze) land. Until then, run `guild project list` to
#       see lingering registrations — they are harmless but visible.
set -uo pipefail

PROJECT_A="guild-example-03-project-a"
PROJECT_B="guild-example-03-project-b"
SANDBOX_A="/tmp/${PROJECT_A}"
SANDBOX_B="/tmp/${PROJECT_B}"

echo "removing sandbox directories..."
set +e
rm -rf "${SANDBOX_A}"
rm -rf "${SANDBOX_B}"
set -e

echo "teardown complete"
echo "  NOTE: project registrations for '${PROJECT_A}' and '${PROJECT_B}' remain"
echo "        in ~/.guild/*.db until QUEST-152 + QUEST-153 land."
