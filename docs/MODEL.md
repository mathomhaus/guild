# Model build and pinning

Maintainer-facing notes on how guild produces the int8 ONNX model that
ships inside the `-tags=withembed` release binary, and how the binary
release pins a specific model version.

## Two-workflow split

Model production and binary release are decoupled.

- `.github/workflows/build-model.yml` runs the recipe, computes
  provenance, and publishes a `model-v<semver>` GitHub Release with
  `model.onnx`, `vocab.txt`, `tokenizer.json`, and `MANIFEST.txt` as
  assets. Pre-release flag is set so it never becomes 'latest'.
- `.github/workflows/release.yml` (the existing tag-driven binary
  release) reads `.model-version` at the repo root, downloads the
  matching model release via `gh`, runs `make assets-model`, and lets
  goreleaser cut the binary archives.

This keeps the binary release path pure-Go plus `curl` plus `gh`.
Python only runs when the model itself is being rebuilt, which is
rare.

## Bumping `.model-version`

`.model-version` is a single-line semver string (e.g. `1.0.0`). It
pins which `model-v<semver>` release the binary release embeds.

Bumping it is a deliberate maintainer PR:

1. Trigger `build-model` (manual or on a recipe push). Verify the new
   `model-v<NEW>` release lands with `MANIFEST.txt`.
2. Open a PR that updates `.model-version` to `NEW`.
3. Merge.
4. The next `vX.Y.Z` tag push for guild produces a binary that embeds
   the new model.

## Triggering a manual model rebuild

```
gh workflow run build-model.yml \
  --ref main \
  --field version=1.0.1
```

The `version` input is optional. When omitted, the workflow reads
`.model-version` and bumps the patch component. The version is the
tag (without the `model-v` prefix), so passing `1.0.1` produces
`model-v1.0.1`.

The workflow refuses to overwrite an existing `model-v<version>` tag,
so re-running with the same version requires deleting the prior
release first.

## Schedule cadence

Cron `0 0 1 */3 *` runs at 00:00 UTC on the 1st of every 3rd month
(Jan, Apr, Jul, Oct). Each run bumps the patch component and
publishes a fresh `model-v<semver>` pre-release. The intent is to
catch upstream BAAI changes and verify that the recipe still produces
a healthy artifact, not to auto-promote new models into the binary
release. The maintainer reviews scheduled outputs and only updates
`.model-version` when the new model passes whatever quality bar
applies.

## MANIFEST.txt provenance

Every `model-v<semver>` release ships a `MANIFEST.txt` asset:

- SHA256 of `model.onnx`, `vocab.txt`, `tokenizer.json`
- pinned `optimum`, `onnxruntime`, `transformers` versions
- BAAI source revision (resolved via `huggingface_hub` at build time)
- build timestamp
- workflow run URL

To audit a binary release, find its `.model-version` value, open the
matching `model-v<version>` release on GitHub, read `MANIFEST.txt`.

## Reverting a degraded model

If a new model release degrades retrieval quality after `.model-version`
has been bumped:

1. Open a PR setting `.model-version` back to the prior good tag.
2. Merge.
3. Re-cut the binary release (push a new `vX.Y.Z` tag, or re-run the
   `release` workflow on the existing tag if that path is enabled).

The `model-v<bad>` GitHub Release can stay; only the pin matters. Keep
the bad release around as a record so the cause can be investigated.

## First-time bootstrap

The first `model-v1.0.0` release does not exist yet. The maintainer
creates it post-merge of this change with:

```
gh workflow run build-model.yml --ref main --field version=1.0.0
```

After it lands, the next `vX.Y.Z` tag push for guild produces a
release that includes the embedded model.
