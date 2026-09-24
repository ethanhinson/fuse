#!/usr/bin/env bash
# Thin wrapper: pins the Python env the vendored LiveCodeBench fork needs and
# forwards every argument to bench.py. Examples:
#   ./run.sh --model minimax --arm single --n 100
#   ./run.sh --model kimi    --arm fuse   --n 100 --parallel 4
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
[ -d "$HERE/vendor/LiveCodeBench" ] || "$HERE/setup.sh"
export LCB_RELEASE="${LCB_RELEASE:-release_v6}"
exec uv run --no-project --python 3.12 \
  --with 'datasets<4' --with numpy --with pyyaml \
  python "$HERE/bench.py" "$@"
