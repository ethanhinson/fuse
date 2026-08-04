---
id: 8
slug: mcp-integration-test-harness
title: MCP Integration Test Harness (Docker Compose + Playwright)
status: in-progress
priority: medium
type: chore
created: 2026-08-04
updated: 2026-08-04
depends_on: [7]
related: [3, 7]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0008-mcp-integration-test-harness.md
plan:
results:
trivial: false
auto_groomable: false
branch: feat/mcp-integration-test-harness
claimed_at: 2026-08-04T18:55:37Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0008-mcp-integration-test-harness.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0008-mcp-integration-test-harness.md) |
<!-- docket:artifacts:end -->

## Why

The MCP client stack (stdio, HTTP/SSE, bearer, OAuth2 PKCE) was built change-by-change with unit tests using in-process stubs. No test today exercises the real wire protocol against a live MCP server, a real OAuth2 authorization flow, or the `fuse mcp-server` subprocess end-to-end. Regressions in transport selection, token caching, refresh, or the PKCE exchange would not be caught until a user hits them.

## What changes

- Docker Compose file (`internal/mcp/testdata/`) with two services: `@modelcontextprotocol/server-everything` (the official MCP reference/test server over HTTP/SSE) and `ghcr.io/navikt/mock-oauth2-server`.
- `//go:build integration`-tagged tests in `internal/mcp/` covering four scenarios: stdio (`fuse mcp-server` subprocess), HTTP no auth, HTTP bearer token, and HTTP OAuth2 with Playwright (`playwright-go`) driving the full browser authorization flow.
- In-process `httptest.Server` bearer and OAuth2 proxy helpers — no extra Docker containers for auth.
- GitHub Actions workflow (`.github/workflows/integration.yml`) that installs Playwright Chromium, starts Compose, and runs `go test -tags integration`.
- `make test-integration` target.

## Out of scope

- Changes to non-test production code.
- mTLS / client-certificate auth.
- Load testing.
- `fuse mcp-server` HTTP transport (not yet implemented).

## Open questions

None — design fully specified in the linked spec.
