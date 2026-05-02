# Embedded runtime assets

Per-platform bundle for the `-tags=withembed` release build. Each
subdirectory corresponds to one `GOOS_GOARCH` triple and holds three
files:

```
libonnxruntime.dylib | libonnxruntime.so
model.onnx
vocab.txt
```

The binary sizes make committing these to git wasteful (roughly 65 MB
per platform, four platforms for a full matrix). Instead, they are
populated on demand by the Makefile target `make assets` before a
release build, and a `.gitignore` in this directory excludes them from
the repo.

## Build tags

The package compiles in two modes:

- **Default (no `-tags`)**: zero assets, `HasAssets() == false`. Every
  `guild init` run writes `meta.embedder_state='disabled'` with reason
  `no_assets`. `make check` works on a fresh clone with no assets on
  disk.
- **`-tags=withembed`**: the per-platform `manifest_bundled_<goos>_<goarch>.go`
  file takes over and `go:embed` pulls in the three files from this
  directory. If any file is missing, the build fails loudly.

Release CI sets `-tags=withembed` after running `make assets`.

## Supported platforms

- `darwin_arm64/`
- `darwin_amd64/`
- `linux_amd64/`
- `linux_arm64/`

Windows is deliberately excluded: `onnxruntime-purego` uses
`purego.Dlopen`, which is not available on its Windows surface (spike
friction F11). Windows builds write `embedder_state='disabled'` with
reason `unsupported_platform`.

## Staging the assets locally

```
make assets
```

Pulls `model.onnx` + `vocab.txt` from the spike workspace
(`../lares-spikes/guild-embedding-purego/workspace/models/bge-small-int8/`)
and the `libonnxruntime` tarball from the ONNX Runtime 1.23.x release
on GitHub.

## Regenerating after a model upgrade

If the model or vocab changes, the manifest's `embedder_model_id` or
`embedder_tokenizer_hash` should be bumped and the reference vectors in
`../testdata/reference_vectors.json` regenerated from the Python
pipeline in the spike. The probe will fail otherwise.
