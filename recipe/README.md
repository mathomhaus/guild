# recipe/

Reproducible build recipe for the int8 ONNX model that ships inside
the guild release binary (`-tags=withembed`).

The recipe is the source of truth. The model artifact is downstream:
`recipe/quantize.py` produces it, `.github/workflows/build-model.yml`
publishes it as a GitHub Release, and `.model-version` at the repo
root pins the release tag that the binary release workflow consumes.

## Files

- `quantize.py`: the two-step recipe (optimum-cli export, then
  onnxruntime quantize_dynamic with QInt8). Reads the upstream model
  name as a constant; writes outputs under `workspace/models/`.
- `requirements.txt`: pinned versions of optimum, onnxruntime, and
  the pieces the export pipeline pulls in.

## Run locally

Validates the recipe end-to-end without going through CI. Useful when
bumping `requirements.txt` or eyeballing the int8 SHA256 before
publishing a model release.

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r recipe/requirements.txt
python recipe/quantize.py
```

Outputs:

- `workspace/models/bge-small-fp32/model.onnx` (intermediate, ~127 MB)
- `workspace/models/bge-small-fp32/vocab.txt`
- `workspace/models/bge-small-fp32/tokenizer.json`
- `workspace/models/bge-small-int8/model.onnx` (final, ~33 MB)

Total run time: roughly one minute on a recent laptop, dominated by
the HuggingFace download of the FP32 base model.

## Relationship to `.model-version` and the build-model workflow

```
recipe/quantize.py                 (you are here)
        |
        v
.github/workflows/build-model.yml  (runs the recipe in CI, uploads
        |                           model.onnx, vocab.txt,
        |                           tokenizer.json, MANIFEST.txt as
        |                           model-v<semver> release assets)
        v
.model-version                     (semver string the binary release
        |                           workflow reads)
        v
.github/workflows/release.yml      (downloads model-v$VERSION assets,
                                    runs `make assets-model`, then
                                    goreleaser produces the embed
                                    binary)
```

Triggers for `build-model.yml`:

1. `workflow_dispatch` (manual) with optional `version` input. The
   maintainer runs this once after merging this quest to bootstrap the
   first model release.
2. `push` on `recipe/**` paths. Any change to this directory rebuilds
   the model.
3. `schedule` quarterly (cron `0 0 1 */3 *`). Catches upstream BAAI
   changes and verifies the recipe still produces a healthy artifact.

The output of every model build is auditable via the `MANIFEST.txt`
asset on each `model-v*` release: SHA256s of all files, optimum and
onnxruntime versions used, BAAI source revision, build timestamp, and
the workflow run URL.
