#!/usr/bin/env bash
# run.sh brings up the Wander concierge demo end-to-end:
#   1. builds the @fuse/sdk browser bundle (build.sh, esbuild);
#   2. builds + starts the rentals MCP server (cmd/rentals-mcp) on RENTALS_ADDR
#      (default 127.0.0.1:8091) — the demo's live-data backend;
#   3. builds + starts `fuse loop-serve-net` on FUSE_NET_ADDR (default 127.0.0.1:8787);
#   4. serves the static Wander page (server.js) on PORT (default 5173), reverse-proxying
#      the Connect path to the backend so the browser stays same-origin.
#
# By default it points the loop server at a REAL model gateway via your ~/.fuse/config.yml.
# To run fully offline/deterministically (no provider), set LLM_GATEWAY_URL to a scripted
# double before invoking (the CI browser lane does exactly this; it NEVER calls Claude).
#
# The rentals defaults below MUST agree with examples/wander/fuse.demo.yml, which you merge
# into ~/.fuse/config.yml (see that file's header): fuse mints the delegation token from
# tool_identity.signing_key and dials mcp_servers[rentals].url, and this server verifies
# with the same key at the same address. internal/config's
# TestWanderRunScriptStartsRentalsServer asserts the two sides agree, so drift fails a test
# instead of silently demoing nothing.
#
# Ctrl-C tears all three processes down.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"

FUSE_NET_ADDR="${FUSE_NET_ADDR:-127.0.0.1:8787}"
PORT="${PORT:-5173}"

# --- rentals MCP server knobs — keep in lockstep with fuse.demo.yml ----------------
RENTALS_ADDR="${RENTALS_ADDR:-127.0.0.1:8091}"
RENTALS_AUDIENCE="${RENTALS_AUDIENCE:-https://rentals.demo.fuse.local}"
# DEMO KEY — fake, checked in, grants access to nothing. Equals tool_identity.signing_key.
RENTALS_SIGNING_KEY="${RENTALS_SIGNING_KEY:-demo-not-a-secret-wander-rentals-signing-key}"
# Every tenant that can reach the rentals server: each tenant under loop_server.auth PLUS
# `_default` (the token picker's default option, and what an auth entry with no `tenant:`
# normalizes to). A tenant missing here makes that user's every call unauthorized.
RENTALS_TENANTS="${RENTALS_TENANTS:-_default,acme,globex}"
RENTALS_FAVORITES_DIR="${RENTALS_FAVORITES_DIR:-/tmp/wander-favorites}"
# "auto" = live web-search listings IF a search credential (e.g. TAVILY_API_KEY) is in the
# environment, else canned listings. The demo never requires a credential.
RENTALS_DATA="${RENTALS_DATA:-auto}"

echo "[wander] building SDK browser bundle…"
"$here/build.sh"

bin_dir="$(mktemp -d)"
fuse_bin="$bin_dir/fuse"
rentals_bin="$bin_dir/rentals-mcp"

echo "[wander] building fuse + rentals-mcp binaries…"
( cd "$repo_root" && go build -o "$fuse_bin" ./cmd/fuse )
( cd "$repo_root" && go build -o "$rentals_bin" ./cmd/rentals-mcp )

cleanup() {
  [[ -n "${static_pid:-}" ]] && kill "$static_pid" 2>/dev/null || true
  [[ -n "${fuse_pid:-}" ]] && kill "$fuse_pid" 2>/dev/null || true
  [[ -n "${rentals_pid:-}" ]] && kill "$rentals_pid" 2>/dev/null || true
  rm -rf "$bin_dir"
}
trap cleanup EXIT INT TERM

echo "[wander] starting rentals MCP server on $RENTALS_ADDR (data=$RENTALS_DATA)…"
RENTALS_ADDR="$RENTALS_ADDR" \
RENTALS_AUDIENCE="$RENTALS_AUDIENCE" \
RENTALS_SIGNING_KEY="$RENTALS_SIGNING_KEY" \
RENTALS_TENANTS="$RENTALS_TENANTS" \
RENTALS_FAVORITES_DIR="$RENTALS_FAVORITES_DIR" \
RENTALS_DATA="$RENTALS_DATA" \
  "$rentals_bin" &
rentals_pid=$!

echo "[wander] starting fuse loop-serve-net on ${FUSE_NET_ADDR}…"
( cd "$repo_root" && "$fuse_bin" loop-serve-net --addr "$FUSE_NET_ADDR" ) &
fuse_pid=$!

# Give the backends a moment to bind.
sleep 1

# server.js binds 127.0.0.1 ONLY (it publishes demo bearer tokens and reverse-proxies them
# into the loop backend, so it must never be LAN-reachable). To view the demo from another
# machine, tunnel: ssh -L "${PORT}:127.0.0.1:${PORT}" this-box.
echo "[wander] serving the page on http://127.0.0.1:${PORT}…"
FUSE_NET_ADDR="$FUSE_NET_ADDR" PORT="$PORT" node "$here/server.js" &
static_pid=$!

wait "$static_pid"
