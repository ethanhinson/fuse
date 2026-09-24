#!/usr/bin/env bash
# Fetch upstream LiveCodeBench (MIT) at a pinned commit and apply our patches.
# It supplies the dataset loader and the hidden-test evaluator; the problem
# selection, prompts, judges and agent harness live in this directory.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="https://github.com/LiveCodeBench/LiveCodeBench.git"
PIN="28fef95ea8c9f7a547c8329f2cd3d32b92c1fa24"   # main as of 2025-07-16
DST="$HERE/vendor/LiveCodeBench"

if [ -d "$DST/.git" ]; then
  echo "LiveCodeBench already present at $DST ($(git -C "$DST" rev-parse --short HEAD))"
else
  mkdir -p "$HERE/vendor"
  git clone -q --filter=blob:none "$REPO" "$DST"
  git -C "$DST" checkout -q "$PIN"
  for p in "$HERE"/patches/*.patch; do
    git -C "$DST" apply "$p"
    echo "applied $(basename "$p")"
  done
  echo "LiveCodeBench @ ${PIN:0:12} -> $DST"
fi
command -v uv >/dev/null || { echo "uv is required (https://docs.astral.sh/uv/)" >&2; exit 1; }
echo "ok. First run downloads LiveCodeBench release_v6 from Hugging Face (~3 min)."
