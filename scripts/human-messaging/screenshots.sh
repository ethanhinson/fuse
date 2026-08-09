#!/usr/bin/env bash
# Regenerate the human-messaging UI frames into reports/screenshots/.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
export PATH="$(go env GOPATH)/bin:$PATH"
DIR="$PWD/reports/screenshots"
mkdir -p "$DIR"
FUSE_SCREENSHOT_DIR="$DIR" go test ./internal/tui/ \
  -run 'TestScreenshot_Human|TestScreenshot_Queue|TestScreenshot_Btw' -count=1
echo "Frames written to $DIR:"
ls -1 "$DIR"/human-*.txt "$DIR"/human-*.png 2>/dev/null || true
