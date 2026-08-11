#!/usr/bin/env bash
# Regenerate the committed loop.* wire stubs (Go + TS) from proto/fuse/loop/v1.
# Generate-and-commit: CI is hermetic (no live codegen); this script is the drift
# source. Requires buf + protoc-gen-go + protoc-gen-connect-go on PATH and the TS
# plugins installed under proto/ (npm ci). See docs for pinned versions.
set -euo pipefail
cd "$(dirname "$0")"
buf lint
buf generate
