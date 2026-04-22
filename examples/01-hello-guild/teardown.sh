#!/usr/bin/env bash
# Clean up the sandbox project for example 01-hello-guild.
# Best-effort: removes the sandbox directory on disk.
#
# NOTE: project registration in ~/.guild/*.db is NOT removed here.
# Full single-project teardown is blocked on:
#   QUEST-152 — lore strike / quest strike (bulk-delete by project)
#   QUEST-153 — guild raze (full project de-registration)
# Until those land, the entry persists in cross-project lists but is harmless.
set -euo pipefail

SANDBOX="/tmp/guild-example-01-hello-guild"

if [[ -d "${SANDBOX}" ]]; then
  rm -rf "${SANDBOX}"
  echo "removed ${SANDBOX}"
else
  echo "${SANDBOX} not found — nothing to remove"
fi

echo "teardown complete (project registration in ~/.guild/*.db persists until QUEST-152/153 land)"
