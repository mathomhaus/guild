#!/bin/sh
# GoReleaser OSS v2.15 matches ignore_tags exactly when tags point at HEAD.
# Enumerate all model tags so neither current nor previous binary versions
# can accidentally resolve to a model release. The repository is read-only.
set -eu
base=${1:?usage: goreleaser-config.sh BASE OUTPUT}
output=${2:?usage: goreleaser-config.sh BASE OUTPUT}
[ "$base" != "$output" ] || { echo 'base and output must differ' >&2; exit 1; }
if grep -q '^git:' "$base"; then
    echo 'base config must leave git settings to this generator' >&2
    exit 1
fi
tags=$(git tag --list 'model-v*')
mkdir -p "$(dirname "$output")"
cat "$base" > "$output"
printf '\n# Generated exact model-tag exclusions; do not edit this copy.\ngit:\n' >> "$output"
if [ -z "$tags" ]; then
    printf '  ignore_tags: []\n' >> "$output"
else
    printf '  ignore_tags:\n' >> "$output"
    printf '%s\n' "$tags" | while IFS= read -r tag; do
        escaped=$(printf '%s' "$tag" | sed "s/'/''/g")
        printf "    - '%s'\n" "$escaped" >> "$output"
    done
fi
