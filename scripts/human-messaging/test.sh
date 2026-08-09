#!/usr/bin/env bash
# Run every test backing the human-messaging features (ADR-0022).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
echo "== substrate (bus, handles, /btw parser, injector, completion hook) =="
go test ./internal/agent/ -run 'TestHuman|TestHandle|TestParseAside|TestAnswerAside|TestSanitize|TestLoopInjects' -count=1
echo "== routing + shell integration =="
go test ./internal/tui/ -run 'TestClassify|TestShellRoute|TestQueueEditor|TestAskChatAboutThis' -count=1
echo "== router parser =="
go test ./cmd/fuse/ -run 'TestParseRouteDecision' -count=1
echo "ALL HUMAN-MESSAGING TESTS PASSED"
