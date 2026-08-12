#!/usr/bin/env bash
# run.sh brings up the Wander concierge demo end-to-end:
#   1. builds the @fuse/sdk browser bundle (build.sh, esbuild);
#   2. builds + starts `fuse loop-serve-net` on FUSE_NET_ADDR (default 127.0.0.1:8787);
#   3. serves the static Wander page (server.js) on PORT (default 5173), reverse-proxying
#      the Connect path to the backend so the browser stays same-origin.
#
# By default it points the loop server at a REAL model gateway via your ~/.fuse/config.yml.
# To run fully offline/deterministically (no provider), set LLM_GATEWAY_URL to a scripted
# double before invoking (the CI browser lane does exactly this; it NEVER calls Claude).
#
# Ctrl-C tears both processes down.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"

FUSE_NET_ADDR="${FUSE_NET_ADDR:-127.0.0.1:8787}"
PORT="${PORT:-5173}"

echo "[wander] building SDK browser bundle…"
"$here/build.sh"

echo "[wander] building fuse binary…"
fuse_bin="$(mktemp -d)/fuse"
( cd "$repo_root" && go build -o "$fuse_bin" ./cmd/fuse )

echo "[wander] starting fuse loop-serve-net on $FUSE_NET_ADDR…"
( cd "$repo_root" && "$fuse_bin" loop-serve-net --addr "$FUSE_NET_ADDR" ) &
fuse_pid=$!

cleanup() {
  kill "$fuse_pid" 2>/dev/null || true
  [[ -n "${static_pid:-}" ]] && kill "$static_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Give the backend a moment to bind.
sleep 1

echo "[wander] serving the page on http://localhost:$PORT…"
FUSE_NET_ADDR="$FUSE_NET_ADDR" PORT="$PORT" node "$here/server.js" &
static_pid=$!

wait "$static_pid"
