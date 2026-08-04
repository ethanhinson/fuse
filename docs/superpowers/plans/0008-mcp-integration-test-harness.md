<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0008 — MCP Integration Test Harness (Docker Compose + Playwright)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0008-mcp-integration-test-harness.md)**
<!-- docket:backlink:end -->

# Plan — MCP Integration Test Harness (change 0008)

Authoritative spec: change 0008 (`docs/superpowers/specs/0008-mcp-integration-test-harness.md` on the `docket` branch). This plan is executed on `feat/mcp-integration-test-harness`, cut from `origin/main` (`9b566ec`).

## Ground rules for every task

- **Zero production-code changes.** The `case "mcp-server"` CLI dispatch is already on `origin/main`. Only test code, testdata, CI, and Make are added.
- All Go test files carry the `//go:build integration` build tag on the first line, followed by a blank line, so the default (`go test ./...`) build is unaffected.
- Tests are **in-package** (`package mcp`) — they use unexported `newStdioClient`, `newHTTPClient`, and the `mcpConn` interface.
- **Graceful skip:** every Docker/service-dependent test and `TestMain`'s compose lifecycle must `t.Skip(...)` (or set a package-level "services unavailable" flag that each dependent test checks) when `docker` is absent, `docker compose up` fails, or a health endpoint never comes up. The stdio test has **no** Docker dependency and must run without it. Playwright's browser install failing must also skip, not fail, the OAuth2 test.
- Verification after each task: `go build ./...` and `go vet ./...` stay green (unchanged, since integration files are tag-excluded), and `go build -tags integration ./internal/mcp/...` compiles. Full `go test -tags integration ./internal/mcp/...` is only expected green when Docker + Playwright are available; otherwise it must **skip** cleanly (no failures, no hangs).

## Task 1 — Add the `playwright-go` dependency

**Goal:** `github.com/playwright-community/playwright-go` is a module dependency, resolvable, used only under the integration tag.

- Add the import in a throwaway or the eventual OAuth2 test file, then `go mod tidy` (or `go get github.com/playwright-community/playwright-go@latest`).
- Verify: `go build ./...` unaffected; `go build -tags integration ./internal/mcp/...` resolves the module. `go.mod`/`go.sum` updated.

**Done when:** module is in `go.mod`, `go mod verify` passes, non-integration build unchanged.

## Task 2 — Compose testdata files

**Goal:** the Docker Compose stack and mock-oauth2 config exist as testdata.

- Write `internal/mcp/testdata/docker-compose.yml` with two services per the spec:
  - `mcp-everything` (`node:22-alpine`, `npx --yes @modelcontextprotocol/server-everything --transport sse --port 3001`, port 3001, `/sse` healthcheck).
  - `mock-oauth2` (`ghcr.io/navikt/mock-oauth2-server:2.2.1`, port 8080, `JSON_CONFIG_PATH=/config.json`, mounting `./mock-oauth2-config.json`, `.well-known/openid-configuration` healthcheck).
- Write `internal/mcp/testdata/mock-oauth2-config.json`: a single `default` issuer, auto-approve/login enabled so Playwright's redirect needs no interactive login form (or a trivially auto-submittable one).
- Verify: `docker compose -f internal/mcp/testdata/docker-compose.yml config` parses (skip if `docker` absent). Files are valid YAML/JSON.

**Done when:** both testdata files exist and parse; no Go code yet.

## Task 3 — `TestMain` + compose lifecycle + shared helpers (`integration_test.go`)

**Goal:** `internal/mcp/integration_test.go` (package `mcp`, `//go:build integration`) owns process-wide setup/teardown and the shared proxy helpers.

- `TestMain(m)`:
  - Detect Docker (`exec.LookPath("docker")` + `docker compose version`). If absent → set a package `servicesUp = false`, still run `m.Run()` (so the stdio test executes), skip teardown.
  - If present: `docker compose -f testdata/docker-compose.yml up -d`, poll `http://localhost:3001/sse` and `http://localhost:8080/default/.well-known/openid-configuration` until healthy or a bounded timeout; on success `servicesUp = true`. `defer docker compose down -v`.
  - Initialize Playwright once (`playwright.Install()` best-effort + `playwright.Run()`); on failure set `playwrightUp = false` (OAuth2 test skips), never fail `TestMain`.
- Helper `requireServices(t)` → `t.Skip` when `!servicesUp`.
- `newBearerProxy(t *testing.T, upstream *url.URL, token string) *httptest.Server` — enforces `Authorization: Bearer <token>`, 401 on wrong/missing, else reverse-proxies (SSE-safe via flush/hijack), closes on `t.Cleanup`.
- `newOAuthProxy(t *testing.T, upstream *url.URL, issuerURL string) *httptest.Server` — fetches JWKS from `{issuerURL}/default/jwks.json`, validates `alg`/`iss`/`exp` on each request, else 401; proxies on success; closes on `t.Cleanup`.
- Verify: `go build -tags integration ./internal/mcp/...` compiles; `go vet -tags integration ./internal/mcp/...` clean.

**Done when:** file compiles under the tag, `TestMain` runs and skips gracefully without Docker.

## Task 4 — Scenario 1: stdio `TestIntegration_Stdio` (`stdio_integration_test.go`)

**Goal:** spawn `fuse mcp-server` and assert the tool list. **No Docker dependency** — runs unconditionally.

- TDD: write the test first. Resolve the `fuse` binary: `$FUSE_BINARY` → `go build -o` into `t.TempDir()` → fall back to `go run ./cmd/fuse`.
- Construct the client via in-package `newStdioClient("test", []string{<fuse-bin>, "mcp-server"}, nil)`.
- Call `initialize` then `tools/list`; assert the result contains the `bash` tool. Assert tools are **listed, not executed** (permission gate wired).
- `defer client.stop()`.
- Verify: `go test -tags integration -run TestIntegration_Stdio ./internal/mcp/...` passes with no services running.

**Done when:** the stdio test passes standalone (no Docker), green.

## Task 5 — Scenario 2: HTTP no-auth `TestIntegration_HTTP_NoAuth` (`http_noauth_integration_test.go`)

**Goal:** dial `mcp-everything` directly, exercise `tools/list` + `tools/call echo`.

- TDD: `requireServices(t)` first. `newHTTPClient("everything", "http://localhost:3001", "")`.
- `tools/list` → assert `echo` and `add` present. `tools/call` `echo` → assert round-trip result.
- Verify (with services up): test passes; without Docker: skips.

**Done when:** compiles and skips-or-passes correctly.

## Task 6 — Scenario 3: HTTP bearer `TestIntegration_HTTP_Bearer` (`http_bearer_integration_test.go`)

**Goal:** bearer auth through the in-process proxy.

- `requireServices(t)`. Start `newBearerProxy` in front of `http://localhost:3001` with token `test-token-xyz`.
- Sub-test `wrongToken`: build `newHTTPClient` with a bad token → expect 401 from the proxy.
- Sub-test `correctToken`: `config.MCPAuthConfig{Type: "bearer", ClientSecret: "test-token-xyz"}` → `GetAccessToken` returns `ClientSecret` → `newHTTPClient(name, proxyURL, token)` → `tools/call` succeeds.
- Verify: skips-or-passes correctly.

**Done when:** both sub-tests green with services up.

## Task 7 — Scenario 4: HTTP OAuth2 + Playwright `TestIntegration_HTTP_OAuth2` (`http_oauth2_integration_test.go`)

**Goal:** full browser authorization flow + silent refresh.

- `requireServices(t)`; also skip when `!playwrightUp`.
- Start `newOAuthProxy` in front of `http://localhost:3001` with issuer `http://localhost:8080`.
- Config: `config.MCPAuthConfig{Type: "oauth2", ClientID: "fuse-test", ClientSecret: "secret", Scopes: []string{"openid"}, TokenFile: filepath.Join(t.TempDir(), "tokens.json")}`.
- Drive `GetAccessToken("everything", proxyURL, cfg)`: it starts the local callback server + opens the auth URL; Playwright (headless Chromium) navigates the URL, mock-oauth2 auto-approves, redirect delivers the code, `GetAccessToken` exchanges for tokens, persists to `TokenFile`.
- Assert: client `tools/list` through the OAuth2 proxy succeeds.
- Refresh sub-test: artificially expire the persisted token (rewrite `TokenFile`), next call triggers silent `refresh_token` (no browser) and succeeds.
- Verify: skips cleanly when Docker or Playwright unavailable; passes when both present.

**Done when:** compiles; skips-or-passes correctly.

## Task 8 — CI workflow `.github/workflows/integration.yml`

**Goal:** net-new GitHub Actions job (no `.github/` exists yet).

- Create `.github/workflows/integration.yml` per the spec: checkout, `setup-go` from `go.mod`, install Playwright Chromium (`--with-deps`), `docker compose up -d`, wait-for-services loop, `go test -tags integration -v -timeout 120s ./internal/mcp/...`, `down -v` in `always()`.
- Verify: YAML parses (e.g. `yq`/`python -c yaml.safe_load`), job/step structure matches spec.

**Done when:** workflow file exists and is valid YAML.

## Task 9 — Makefile `test-integration` target

**Goal:** append a `test-integration` target to the existing root `Makefile`.

- Append `.PHONY: test-integration` and the target that brings compose up, runs `go test -tags integration -v -timeout 120s ./internal/mcp/...`, and tears compose down (down runs even on test failure).
- Verify: `make -n test-integration` prints the recipe; existing targets (`build`/`install`/`test`/`lint`) untouched.

**Done when:** target present, `make -n test-integration` works, other targets unchanged.

## Final gate

- `go build ./...` and `go test ./...` (no tag) green and unchanged.
- `go build -tags integration ./internal/mcp/...` compiles; `go vet -tags integration ./internal/mcp/...` clean.
- `go test -tags integration ./internal/mcp/...` in a Docker-less environment **skips** all service-dependent tests and **passes** the stdio test — no failures, no hangs.
- No production `.go` file outside `_test.go` is modified.
