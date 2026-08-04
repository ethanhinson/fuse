<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0008 — MCP Integration Test Harness (Docker Compose + Playwright)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-04-0008-mcp-integration-test-harness.md)**
<!-- docket:backlink:end -->

# MCP Integration Test Harness

**Spec for change 0008**

---

## Overview

A `//go:build integration`-tagged test suite in `internal/mcp/` that exercises the full MCP client stack end-to-end against real infrastructure: a reference MCP server, a mock OAuth2 server, and the `fuse mcp-server` subprocess. Docker Compose provisions the external services; `TestMain` starts and tears them down. Playwright (`playwright-go`) drives the OAuth2 browser authorization flow both locally and in CI.

---

## Docker Compose services

File: `internal/mcp/testdata/docker-compose.yml`

Two services only — auth proxies are in-process to avoid extra containers.

### `mcp-everything`

```yaml
mcp-everything:
  image: node:22-alpine
  # transport is a positional arg ([stdio|sse|streamableHttp]); `sse` binds :3001.
  command: >
    sh -c "npx --yes @modelcontextprotocol/server-everything sse"
  ports:
    - "3001:3001"
  healthcheck:
    test: ["CMD", "wget", "-qO-", "http://localhost:3001/sse"]
    interval: 2s
    timeout: 5s
    retries: 15
```

`@modelcontextprotocol/server-everything` is the official MCP reference/test server. It exposes tools (`echo`, `add`, `longRunningOperation`, etc.), resources, and prompts — no external API key required. `--transport sse` activates HTTP/SSE mode.

### `mock-oauth2`

```yaml
mock-oauth2:
  image: ghcr.io/navikt/mock-oauth2-server:2.2.1
  ports:
    - "8080:8080"
  environment:
    - JSON_CONFIG_PATH=/config.json
  volumes:
    - ./mock-oauth2-config.json:/config.json:ro
  # No container healthcheck — distroless image (no shell/wget). Readiness is
  # gated host-side (TestMain's waitForServices, the CI wait loop, and make).
```

`mock-oauth2-config.json` sets `interactiveLogin: false` on a single `default` issuer, so the authorize endpoint auto-redirects to the callback with a code (no user interaction). The Playwright step still exercises the real redirect → callback → token exchange flow.

---

## In-process test proxies

Rather than adding Docker containers for bearer and OAuth2 auth scenarios, thin `httptest.Server` wrappers sit in front of `mcp-everything`.

### Bearer proxy

```go
// newBearerProxy returns an httptest.Server that enforces Authorization: Bearer <token>
// then reverse-proxies to upstream. Closes when t ends.
func newBearerProxy(t *testing.T, upstream *url.URL, token string) *httptest.Server
```

Responses on wrong / missing token: `401 Unauthorized`. Correct token: proxied verbatim (SSE streams included, via `http.Hijack`).

### OAuth2 proxy

```go
// newOAuthProxy returns an httptest.Server that validates a JWT Bearer token
// against the mock-oauth2 JWKS endpoint then proxies to upstream.
func newOAuthProxy(t *testing.T, upstream *url.URL, issuerURL string) *httptest.Server
```

Fetches JWKS from `{issuerURL}/default/jwks.json` at startup, caches keys, validates `alg`/`iss`/`exp` on each request.

---

## Test scenarios

All tests live in `internal/mcp/` with the `//go:build integration` tag. They are **in-package** (`package mcp`, not `mcp_test`) because the transport constructors and client types they exercise are **unexported**: `newStdioClient(name string, command []string, env []string) (*StdioClient, error)` and `newHTTPClient(name, baseURL, bearerToken string) (*httpClient, error)`, both satisfying the unexported `mcpConn` interface (`call`, `stop`) in `conn.go`. `TestMain` runs `docker compose -f testdata/docker-compose.yml up -d`, polls health endpoints, defers `docker compose down`.

### 1 — Stdio: `fuse mcp-server`

```
TestIntegration_Stdio
```

Spawns the `fuse` binary with the `mcp-server` subcommand via the in-package `newStdioClient` constructor. Calls `initialize` + `tools/list`, asserts that the response includes at least the `bash` tool (a known built-in). Verifies the permission gate is wired (tools are listed, not executed). No Docker dependency.

Binary lookup order: `$FUSE_BINARY` env var → `go build -o` into `t.TempDir()` → `go run ./cmd/fuse`.

**Reconcile note (2026-08-04, second pass) — CLI dispatch is now already wired on `origin/main`.** The prior reconcile pass planned a one-line production touch (`case "mcp-server"` in `cmd/fuse/main.go`) because that dispatch was missing on the base branch. It is **no longer missing**: the `mcp-server` implementation (`internal/hitl/`, `internal/mcp/server.go`, `cmd/fuse/cli_adapter.go`) plus the `case "mcp-server": return runMCPServer(args[1:], cfg, stdout, stderr)` routing were committed to `origin/main` (commit `9b566ec`, "feat: add mcp-server subcommand, HITL relay, and CLI adapter") — this was the very work whose absence halted the prior run. `go build ./...` on `origin/main` now passes, and `fuse mcp-server` is reachable via the CLI as shipped. **This change therefore touches ZERO production code** — it is purely test code, testdata, CI, and Make. The stdio test spawns `fuse mcp-server` and exercises the already-wired path unchanged. (`runMCPServer` registers `defaultToolRegistry(nil)`, whose `DefaultTools()` includes the `bash` tool asserted below.)

### 2 — HTTP, no auth

```
TestIntegration_HTTP_NoAuth
```

Dials `httpClient` directly to `mcp-everything` at `http://localhost:3001`. Calls `tools/list`, asserts `echo` and `get-sum` tools appear (reconcile: the current `server-everything` exposes `get-sum`, not `add`). Calls `tools/call` with `echo` (result text `"Echo: <message>"`) and `get-sum` (`{a,b}` → sum) → verifies both round-trips. No auth config.

### 3 — HTTP, bearer token

```
TestIntegration_HTTP_Bearer
```

Starts `newBearerProxy` in front of `mcp-everything`. **Reconcile note — `MCPAuthConfig` has no `Token` field.** For `Type: "bearer"` the token is carried in `ClientSecret` (see `internal/config/schema.go`: `ClientSecret` is documented "used as bearer token for type=bearer"). Configure:

```go
config.MCPAuthConfig{Type: "bearer", ClientSecret: "test-token-xyz"}
```

`GetAccessToken(serverName, serverURL, cfg)` returns `cfg.ClientSecret` for the bearer type; that value is then handed to `newHTTPClient(name, baseURL, bearerToken)`. Sub-test `wrongToken`: build the client with a bad bearer token, expect the proxy to return 401. Sub-test `correctToken`: correct token, `tools/call` succeeds.

### 4 — HTTP, OAuth2 (Playwright)

```
TestIntegration_HTTP_OAuth2
```

Starts `newOAuthProxy` in front of `mcp-everything`. Config (the struct is `config.MCPAuthConfig`; all fields below exist verbatim on the integration branch):

```go
config.MCPAuthConfig{
    Type:         "oauth2",
    ClientID:     "fuse-test",
    ClientSecret: "secret",
    Scopes:       []string{"openid"},
    TokenFile:    filepath.Join(t.TempDir(), "tokens.json"),
}
```

`GetAccessToken(serverName, serverURL string, cfg config.MCPAuthConfig) (string, error)` routes `type=oauth2` through the browser flow. `TokenFile` is the override consumed by the unexported `tokenFilePath(serverName, cfg.TokenFile)` — when set (as here) it is used directly; when empty it defaults to `~/.fuse/mcp-tokens/<serverName>.json`. Setting `TokenFile` into `t.TempDir()` keeps the test hermetic (no writes to the real home dir).

Flow:
1. `GetAccessToken` detects no cached token → starts local callback server → **shells out** (`open`/`xdg-open`) to open the authorization URL. It does NOT return that URL to the caller, and production code is not changed. To observe the URL, the test prepends a temp dir to `PATH` holding an `open`/`xdg-open` shim that writes its argument to a file; the test polls that file (**reconcile note — zero-production-change browser-open capture**).
2. Playwright (`playwright-go`) navigates the captured URL in a headless Chromium browser.
3. `mock-oauth2` (`interactiveLogin: false`) auto-redirects to the local callback with a code.
4. `GetAccessToken` exchanges code for tokens, persists to `TokenFile`, returns access token.
5. Client calls `tools/list` through the OAuth2 proxy — succeeds.
6. The persisted access token is expired (direct rewrite of `TokenFile`) → next call triggers silent refresh via `refresh_token` (no browser, no Playwright). The refresh sub-test skips if the issuer returned no `refresh_token`.

Playwright is initialized once in `TestMain` via `playwright.Install()` + `playwright.Run()`, headless in CI. **Reconcile/ADR-0001 — driver CDN.** `playwright-go` is pinned to **v0.5001.0** and `PLAYWRIGHT_DOWNLOAD_HOST` is overridden to the live Microsoft CDN, because that version's default `playwright.azureedge.net` CDN was retired (404 on all platforms) and its bundled driver (1.50.1) is only still served on the Microsoft host. The override is set test-only (in `TestMain` and as a CI job env var); when the driver or Docker is unavailable the OAuth2 test **skips gracefully**. See ADR-0001.

---

## GitHub Actions workflow

File: `.github/workflows/integration.yml`

```yaml
name: Integration
on: [push, pull_request]
jobs:
  mcp-integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Install Playwright browsers
        run: go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium
      - name: Start MCP services
        run: docker compose -f internal/mcp/testdata/docker-compose.yml up -d
      - name: Wait for services
        run: |
          for url in http://localhost:3001/sse http://localhost:8080/default/.well-known/openid-configuration; do
            for i in $(seq 30); do curl -sf "$url" && break || sleep 2; done
          done
      - name: Run integration tests
        run: go test -tags integration -v -timeout 120s ./internal/mcp/...
      - name: Teardown
        if: always()
        run: docker compose -f internal/mcp/testdata/docker-compose.yml down -v
```

---

## Makefile additions

```makefile
.PHONY: test-integration

test-integration:
	docker compose -f internal/mcp/testdata/docker-compose.yml up -d
	go test -tags integration -v -timeout 120s ./internal/mcp/... ; \
	  docker compose -f internal/mcp/testdata/docker-compose.yml down -v
```

---

## New files

| Path | Purpose |
|---|---|
| `internal/mcp/integration_test.go` | `TestMain`, shared helpers (`newBearerProxy`, `newOAuthProxy`, compose lifecycle) |
| `internal/mcp/stdio_integration_test.go` | `TestIntegration_Stdio` |
| `internal/mcp/http_noauth_integration_test.go` | `TestIntegration_HTTP_NoAuth` |
| `internal/mcp/http_bearer_integration_test.go` | `TestIntegration_HTTP_Bearer` |
| `internal/mcp/http_oauth2_integration_test.go` | `TestIntegration_HTTP_OAuth2` |
| `internal/mcp/testdata/docker-compose.yml` | Compose services |
| `internal/mcp/testdata/mock-oauth2-config.json` | mock-oauth2 issuer config |
| `.github/workflows/integration.yml` | CI job |

No new packages. **No production-code touch at all** — the `case "mcp-server"` dispatch is already on `origin/main` (see the stdio reconcile note); everything here is test code, testdata, CI, and Make. The `.github/` directory and the `.github/workflows/integration.yml` file are **net-new** — the integration branch has no `.github/` today. The `test-integration` target is appended to the existing root `Makefile` (which currently exposes `build`/`install`/`test`/`lint` only). Module path is `github.com/ethanhinson/fuse`, Go 1.26.5.

---

## Dependencies to add

- `github.com/playwright-community/playwright-go` — Playwright Go bindings (integration tests only; no effect on non-integration builds due to build tag)

---

## Out of scope

- mTLS / client-certificate auth (not implemented in `oauth.go`)
- Testing the `mcp-server` HTTP transport (it only exposes stdio today)
- Load or chaos testing
- Token revocation flow
- **Any production-code change.** As of `origin/main` `9b566ec` the `mcp-server` implementation (`internal/hitl/`, `internal/mcp/server.go`, `cmd/fuse/cli_adapter.go`) and its `case "mcp-server"` CLI dispatch are already merged, so this change adds no production code — only tests, testdata, CI, and Make.
