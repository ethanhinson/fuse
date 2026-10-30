#!/usr/bin/env bash
# Regenerate the committed wire stubs (Go + TS) from proto/fuse/loop/v1 (fuse.loop.v1,
# Go stubs in internal/loopwire/v1) and proto/fuse/harness/v1 (fuse.harness.v1, change
# 0084, Go stubs in the PUBLIC harness/v1).
# Generate-and-commit: CI is hermetic (no live codegen); this script is the drift
# source. Requires buf + protoc-gen-go + protoc-gen-connect-go on PATH and the TS
# plugins installed under proto/ (npm ci). Pinned versions: protoc-gen-go v1.36.12,
# protoc-gen-connect-go v1.20.0, protoc-gen-es 2.13.0 (proto/package.json):
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
#   go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.20.0
#   go install github.com/bufbuild/buf/cmd/buf@latest
# fuse.harness.v1 imports protovalidate (buf.build/bufbuild/protovalidate, pinned in
# buf.lock); buf fetches it from the Buf Schema Registry on first use.
set -euo pipefail
cd "$(dirname "$0")"
buf lint
buf generate
