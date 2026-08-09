#!/usr/bin/env bash
# Drive the real fuse shell in tmux to demo /btw (offline) and the queue/handles.
# The agent-driven parts need a working gateway in your fuse config.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
BIN="$(mktemp -d)/fuse"
go build -o "$BIN" ./cmd/fuse
S=fuse-hm-demo
tmux kill-session -t "$S" 2>/dev/null || true
tmux new-session -d -s "$S" -x 110 -y 40 "$BIN shell"
sleep 2
echo "--- /btw (works with no model) ---"
tmux send-keys -t "$S" "/btw how many running" Enter; sleep 1.2
tmux send-keys -t "$S" "/btw show tree" Enter; sleep 1.2
tmux capture-pane -t "$S" -p | tail -12
echo "--- open the /queue editor ---"
tmux send-keys -t "$S" "/queue" Enter; sleep 1
tmux capture-pane -t "$S" -p | tail -8
tmux kill-session -t "$S" 2>/dev/null || true
echo "(For @handle + queued delivery against a live subagent, ensure your gateway"
echo " is configured, then: spawn a subagent and use @<label> / a queued message.)"
