#!/usr/bin/env bash
# build.sh bundles @fuse/sdk (sdk/ts/src/index.ts) into a single browser ESM module at
# examples/wander/vendor/fuse-sdk.js, so index.html can `import` the REAL published SDK
# surface (createClient / startLoop / send / observe / isCompletion / onState /
# FuseTerminalError) over connect-web — not relative proto imports. esbuild inlines the
# SDK, @connectrpc/connect-web, and @bufbuild/protobuf; Wander never re-implements the wire.
#
# Requires the repo's npm workspace deps installed (esbuild lives at the repo root's
# node_modules/.bin). Run `npm install` at the repo root first if esbuild is missing.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"

esbuild="$repo_root/node_modules/.bin/esbuild"
if [[ ! -x "$esbuild" ]]; then
  echo "esbuild not found at $esbuild — run 'npm install' at the repo root first" >&2
  exit 1
fi

mkdir -p "$here/vendor"
"$esbuild" "$repo_root/sdk/ts/src/index.ts" \
  --bundle \
  --format=esm \
  --platform=browser \
  --outfile="$here/vendor/fuse-sdk.js"

echo "built $here/vendor/fuse-sdk.js"
