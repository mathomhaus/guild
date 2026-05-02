#!/usr/bin/env python3
# recipe/quantize.py
#
# Two-step recipe that turns the upstream BAAI/bge-small-en-v1.5 model
# into the int8 ONNX artifact guild ships inside its release binary.
#
#   step 1: optimum-cli export ONNX (FP32, feature-extraction task)
#   step 2: onnxruntime.quantization.quantize_dynamic with QInt8 weights
#
# Outputs (relative to repo root):
#
#   workspace/models/bge-small-fp32/model.onnx       (intermediate, ~127 MB)
#   workspace/models/bge-small-fp32/vocab.txt        (tokenizer vocab)
#   workspace/models/bge-small-fp32/tokenizer.json   (fast tokenizer)
#   workspace/models/bge-small-int8/model.onnx       (final, ~33 MB)
#
# The build-model.yml workflow consumes the int8 model.onnx plus the
# vocab.txt and tokenizer.json from the FP32 export dir, computes
# SHA256s, writes MANIFEST.txt, and uploads everything as assets on a
# 'model-v<semver>' GitHub Release.
#
# Run locally:
#
#   python3 -m venv .venv && source .venv/bin/activate
#   pip install -r recipe/requirements.txt
#   python recipe/quantize.py
#
# Pinned dependency versions live in recipe/requirements.txt; bumping
# them is a deliberate maintainer action and triggers a model rebuild
# (recipe/** is a workflow trigger path).

import os
import subprocess
import sys

SRC = "BAAI/bge-small-en-v1.5"
FP32_DIR = "workspace/models/bge-small-fp32"
INT8_DIR = "workspace/models/bge-small-int8"
INT8_PATH = os.path.join(INT8_DIR, "model.onnx")


def fail(msg: str, code: int = 1) -> None:
    print(f"recipe/quantize.py: {msg}", file=sys.stderr)
    sys.exit(code)


def step_export_onnx() -> None:
    """Step 1: run optimum-cli to export the HF model to FP32 ONNX."""
    os.makedirs(FP32_DIR, exist_ok=True)
    cmd = [
        sys.executable,
        "-m",
        "optimum.commands.optimum_cli",
        "export",
        "onnx",
        "--model",
        SRC,
        "--task",
        "feature-extraction",
        FP32_DIR,
    ]
    print(f"[1/2] exporting {SRC} -> {FP32_DIR}/model.onnx (FP32)")
    print(f"      cmd: {' '.join(cmd)}")
    try:
        subprocess.check_call(cmd)
    except subprocess.CalledProcessError as e:
        fail(f"optimum-cli export failed (exit {e.returncode})", code=e.returncode)
    fp32_model = os.path.join(FP32_DIR, "model.onnx")
    if not os.path.isfile(fp32_model):
        fail(f"expected {fp32_model} after optimum-cli export but it is missing")


def step_quantize_int8() -> None:
    """Step 2: dynamic int8 quantization of the FP32 ONNX graph."""
    # Import lazily so the file still py_compiles in environments
    # where onnxruntime is not installed (CI sanity check).
    from onnxruntime.quantization import QuantType, quantize_dynamic

    os.makedirs(INT8_DIR, exist_ok=True)
    fp32_model = os.path.join(FP32_DIR, "model.onnx")
    print(f"[2/2] quantizing {fp32_model} -> {INT8_PATH} (QInt8 weights)")
    quantize_dynamic(
        fp32_model,
        INT8_PATH,
        weight_type=QuantType.QInt8,
    )
    if not os.path.isfile(INT8_PATH):
        fail(f"quantize_dynamic returned but {INT8_PATH} is missing")


def main() -> int:
    step_export_onnx()
    step_quantize_int8()
    print(f"int8 model: {INT8_PATH}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
