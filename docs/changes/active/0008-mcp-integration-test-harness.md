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
plan: docs/superpowers/plans/0008-mcp-integration-test-harness.md
results:
trivial: false
auto_groomable: false
branch: feat/mcp-integration-test-harness
claimed_at: 2026-08-04T19:15:30Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0008-mcp-integration-test-harness.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0008-mcp-integration-test-harness.md) |
| Plan | [0008-mcp-integration-test-harness.md](https://github.com/ethanhinson/fuse/blob/feat/mcp-integration-test-harness/docs/superpowers/plans/0008-mcp-integration-test-harness.md) |
<!-- docket:artifacts:end -->

## Why

The MCP client stack (stdio, HTTP/SSE, bearer, OAuth2 PKCE) was built change-by-change with unit tests using in-process stubs. No test today exercises the real wire protocol against a live MCP server, a real OAuth2 authorization flow, or the `fuse mcp-server` subprocess end-to-end. Regressions in transport selection, token caching, refresh, or the PKCE exchange would not be caught until a user hits them.

## What changes

- Docker Compose file (`internal/mcp/testdata/`) with two services: `@modelcontextprotocol/server-everything` (the official MCP reference/test server over HTTP/SSE) and `ghcr.io/navikt/mock-oauth2-server`.
- `//go:build integration`-tagged tests in `internal/mcp/` covering four scenarios: stdio (`fuse mcp-server` subprocess), HTTP no auth, HTTP bearer token, and HTTP OAuth2 with Playwright (`playwright-go`) driving the full browser authorization flow.
- In-process `httptest.Server` bearer and OAuth2 proxy helpers — no extra Docker containers for auth.
- GitHub Actions workflow (`.github/workflows/integration.yml`, net-new — no `.github/` exists yet) that installs Playwright Chromium, starts Compose, and runs `go test -tags integration`.
- `make test-integration` target (appended to the existing `Makefile`).

**No production code.** The `case "mcp-server"` CLI dispatch (needed for the stdio test to spawn `fuse mcp-server`) is already merged on `origin/main` (commit `9b566ec`), so this change touches only test code, testdata, CI, and Make. See the reconcile log.

## Out of scope

- Any changes to production code. The `mcp-server` implementation and its CLI dispatch are already merged on `origin/main`; this change adds only tests, testdata, CI, and Make.
- mTLS / client-certificate auth.
- Load testing.
- `fuse mcp-server` HTTP transport (not yet implemented).

## Open questions

None — design fully specified in the linked spec.

## Reconcile log

### 2026-08-04

Reconciled against the current integration branch (`origin/main`), the linked spec, related changes (0003 stdio MCP + permission gate; 0007 HTTP/SSE + OAuth), and the actual `internal/mcp/` + `cmd/fuse/` code. Change 0007 (`depends_on: [7]`) is `done`. Findings folded into the spec and this body:

1. **`MCPAuthConfig` has no `Token` field.** The struct lives in `internal/config/schema.go`; for `Type: "bearer"` the token is carried in `ClientSecret` (field comment: "used as bearer token for type=bearer"). The spec's `MCPAuthConfig{Type: "bearer", Token: ...}` snippet was corrected to `config.MCPAuthConfig{Type: "bearer", ClientSecret: ...}`.
2. **`GetAccessToken(serverName, serverURL string, cfg config.MCPAuthConfig)`** — the config type is `config.MCPAuthConfig` (package-qualified). OAuth2 config fields (`ClientID`/`ClientSecret`/`Scopes`/`TokenFile`) match verbatim; `TokenFile` overrides the default `~/.fuse/mcp-tokens/<name>.json` via `tokenFilePath`.
3. **Transport constructors are unexported** (`newStdioClient`, `newHTTPClient`) and the `mcpConn` interface is unexported — so the integration tests must be **in-package** (`package mcp`, not `mcp_test`). Spec updated to state this.
4. **`fuse mcp-server` is not routed on `origin/main`.** `cmd/fuse/mcp_server.go` defines `runMCPServer(...)`, but `cmd/fuse/main.go`'s subcommand `switch` routes only `models` and `shell` — there is no `case "mcp-server"`. The stdio test spawns `fuse mcp-server`, so this change adds the one-line dispatch to `main.go`. This narrows the original "no non-test production code" out-of-scope to permit exactly that line. The uncommitted working-tree variant of this wiring (plus untracked `internal/mcp/server.go`, `cmd/fuse/cli_adapter.go`, `internal/hitl/`) is NOT on `origin/main` and is explicitly excluded — the feature branch is cut fresh from `origin/main`.
5. Confirmed net-new: no `.github/` directory on `origin/main`; `Makefile` has no `test-integration` target. Module `github.com/ethanhinson/fuse`, Go 1.26.5.

No obsolescence, no fundamental invalidation — scope adjusted, design intact. `auto_capture` is disabled, so no stubs minted.

### 2026-08-04 — HALTED at plan/build (blocking precondition: integration branch does not compile)

`docket-implement-next` cut a feature branch from `origin/main` and discovered a hard blocker that prevents reaching a PR, so the run **halted** for a human (the feature worktree + branch were removed since no code landed; the change stays `in-progress` with `claimed_at` refreshed and `branch:` cleared so the reclaim lease can self-heal if abandoned).

**Blocker: `origin/main` does not build.** `go build ./...` on `origin/main` fails:

```
cmd/fuse/mcp_server.go:10:2: no required module provides package github.com/ethanhinson/fuse/internal/hitl
```

`cmd/fuse/mcp_server.go` was committed to `origin/main` (last at `aa33c2e`, change 0006's merge) and imports `internal/hitl` + `internal/permissions` and calls `mcp.NewServer(...)` (which would live in `internal/mcp/server.go`) and `defaultToolRegistry(nil)`. But on `origin/main`:
- `internal/hitl/` **does not exist** (present only as an untracked dir in the primary working tree),
- `internal/mcp/server.go` (defining `mcp.NewServer`) **does not exist** (untracked),
- `cmd/fuse/cli_adapter.go` **does not exist** (untracked),
- `cmd/fuse/main.go` / `run.go` carry uncommitted modifications in the primary tree.

So the `fuse mcp-server` subcommand's implementation is **unmerged work-in-progress** sitting only in the primary working tree — it belongs to its own change/PR that must land on `origin/main` first. Change 0008's entire premise (an end-to-end harness that spawns `fuse mcp-server` and exercises the MCP client stack) cannot be built or even compiled against a base branch that does not compile.

**This is an undeclared dependency, not a scope tweak.** Change 0008's frontmatter only lists `depends_on: [7]` (done). It has an unrecorded hard dependency on the change that commits the `mcp-server` server implementation (`internal/hitl`, `internal/mcp/server.go`, `cmd/fuse/cli_adapter.go`, and the `cmd/fuse/main.go` mcp-server routing). That change must be filed/merged to `origin/main` before 0008 is build-ready.

**Human action required (any one of):**
1. Commit the `mcp-server` implementation (the currently-untracked `internal/hitl/`, `internal/mcp/server.go`, `cmd/fuse/cli_adapter.go`, plus the `main.go` routing) as its own change and merge it to `main` so `go build ./...` passes; then add it to 0008's `depends_on:` and re-run `docket-implement-next` for 0008.
2. Or, if that server code is being tracked under a different in-flight change, merge that change first, then re-run 0008.

The reconcile work above (spec + body corrections) is durable and remains valid for the eventual build.

### 2026-08-04 — RESUME reconcile (prior blocker resolved)

Re-claimed and re-reconciled against the advanced integration branch (`origin/main` now at `9b566ec`, "feat: add mcp-server subcommand, HITL relay, and CLI adapter"). The prior halt's hard blocker is **resolved**: the previously-untracked `mcp-server` implementation — `internal/hitl/`, `internal/mcp/server.go`, `cmd/fuse/cli_adapter.go`, and the `case "mcp-server"` routing in `cmd/fuse/main.go` — is now committed and merged to `origin/main`. Verified on a clean `origin/main` checkout: `go build ./...` exits 0.

**Scope reduced — this change now touches ZERO production code.** The one-line `case "mcp-server"` CLI dispatch the prior pass planned to add to `cmd/fuse/main.go` is **already present on `origin/main`** (`main.go:43`). The former "narrows out-of-scope to permit one production line" carve-out is withdrawn: 0008 is now purely test code, testdata, CI, and Make. Body "What changes"/"Out of scope" and the spec's stdio reconcile note, "New files" summary, and "Out of scope" were all updated to match.

**Signatures re-verified against `origin/main` — all match the spec verbatim:** `newStdioClient(name string, command []string, env []string) (*StdioClient, error)`, `newHTTPClient(name, baseURL, bearerToken string) (*httpClient, error)`, the unexported `mcpConn` interface (`internal/mcp/conn.go`), `GetAccessToken(serverName, serverURL string, cfg config.MCPAuthConfig) (string, error)`, `tokenFilePath(serverName, override string)`, `MCPAuthConfig` (bearer token carried in `ClientSecret`; `ClientID`/`Scopes`/`TokenFile` present), and `runMCPServer(_ []string, cfg config.Config, _ io.Writer, stderr io.Writer) int`. The stdio test's `bash`-tool assertion is valid: `runMCPServer` registers `defaultToolRegistry(nil)` → `DefaultTools()` includes `NewBash()` (`internal/tools/registry.go:71`).

No obsolescence, no fundamental invalidation — design intact, scope narrowed. `auto_capture` is disabled, so no stubs minted; no adjacent follow-up work surfaced that warrants capture.
