#!/usr/bin/env bash
# One-command launcher for the Wander concierge demo.
#
# Starts two processes:
#   1. fuse binding #3  (loop-serve-net) — the networked WS + HTTP loop runtime
#   2. the demo web app (server.js)       — static files + WS/HTTP proxy to (1)
#
# Requirements in the environment:
#   - LLM_GATEWAY_URL / LLM_GATEWAY_KEY   (already exported in this shell)
#   - TAVILY_API_KEY                      (pulled from ~/dev/llm-research-agent/.env
#                                          if not already set, so web_search works)
#
# Ctrl-C tears both down.
set -euo pipefail
cd "$(dirname "$0")"

FUSE_ADDR="${FUSE_NET_ADDR:-127.0.0.1:8787}"
WEB_PORT="${PORT:-5173}"

# --- resolve the fuse binary --------------------------------------------------
# Prefer an explicit FUSE_BIN; else build fuse from the tree this demo sits in
# (the demo lives at <tree>/examples/concierge-demo, so ../.. is a checkout that
# contains ./cmd/fuse — whether that checkout is the main repo or the
# feat/networked-runtime-binding worktree). loop-serve-net must be present in that
# tree; if you run from a tree without it, set FUSE_BIN to a binary that has it.
REPO_ROOT="$(cd ../.. && pwd)"
if [[ -n "${FUSE_BIN:-}" ]]; then
  FUSE=("$FUSE_BIN")
elif [[ -x "$REPO_ROOT/fuse" ]]; then
  FUSE=("$REPO_ROOT/fuse")
else
  echo "building fuse from $REPO_ROOT ..."
  ( cd "$REPO_ROOT" && go build -o /tmp/fuse-concierge ./cmd/fuse )
  FUSE=(/tmp/fuse-concierge)
fi

# --- Tavily key (real web search) --------------------------------------------
if [[ -z "${TAVILY_API_KEY:-}" && -f "$HOME/dev/llm-research-agent/.env" ]]; then
  export TAVILY_API_KEY="$(grep -E '^TAVILY_API_KEY=' "$HOME/dev/llm-research-agent/.env" | cut -d= -f2-)"
fi
export FUSE_RESEARCH_PROVIDER="${FUSE_RESEARCH_PROVIDER:-tavily}"

echo "starting fuse loop-serve-net on $FUSE_ADDR ..."
"${FUSE[@]}" loop-serve-net --addr "$FUSE_ADDR" &
FUSE_PID=$!

cleanup() { kill "$FUSE_PID" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

sleep 1
echo "starting web app on http://localhost:$WEB_PORT ..."
PORT="$WEB_PORT" FUSE_NET_ADDR="$FUSE_ADDR" node server.js
