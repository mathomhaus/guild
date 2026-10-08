#!/bin/sh
# Verify all required pinned model files before they enter embedded assets.
set -eu
directory=${1:?usage: verify-model-assets.sh DIRECTORY}
manifest=$directory/MANIFEST.txt
[ -f "$manifest" ] || { echo 'missing model MANIFEST.txt' >&2; exit 1; }
for asset in model.onnx vocab.txt tokenizer.json; do
    expected=$(awk -v asset="$asset" '$2 == asset && length($1) == 64 && $1 !~ /[^0-9a-f]/ {print $1}' "$manifest")
    [ "${#expected}" -eq 64 ] || { echo "missing or duplicate digest: $asset" >&2; exit 1; }
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$directory/$asset")
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$directory/$asset")
    else
        echo 'SHA256 tool unavailable' >&2
        exit 1
    fi
    actual=${actual%% *}
    [ "$actual" = "$expected" ] || { echo "model checksum mismatch: $asset" >&2; exit 1; }
    printf 'verified %s\n' "$asset"
done
