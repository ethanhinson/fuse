<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0008 — MCP Integration Test Harness (Docker Compose + Playwright)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0008-mcp-integration-test-harness.md)**
<!-- docket:backlink:end -->

# Results — MCP Integration Test Harness (change 0008)

## Summary

Added a `//go:build integration` test suite in `internal/mcp/` that exercises the MCP client stack end-to-end against live infrastructure, plus the Docker Compose stack, a net-new GitHub Actions workflow, and a `make test-integration` target. **Zero production code changed** — the `case "mcp-server"` CLI dispatch was already merged on `origin/main` (`9b566ec`) before this change was built.

All four scenarios were verified passing locally against real services:

| Scenario | Test | Result |
|---|---|---|
| stdio (`fuse mcp-server` subprocess) | `TestIntegration_Stdio` | PASS (no Docker dependency) |
| HTTP no-auth | `TestIntegration_HTTP_NoAuth` | PASS |
| HTTP bearer (in-process proxy) | `TestIntegration_HTTP_Bearer` (wrongToken + correctToken) | PASS |
| HTTP OAuth2 + Playwright + refresh | `TestIntegration_HTTP_OAuth2` | PASS (full browser authorize→callback→token flow) |

The default build is unaffected: `go build ./...` and `go test ./...` (no tag) stay green; integration files are excluded by the build tag.

## Manual checks for the merge gate

The integration suite is **not** part of the default `go test ./...` and requires external infra. To reproduce the full green run locally:

1. Docker must be running (Compose stack: `mcp-everything` + `mock-oauth2`).
2. For the OAuth2 scenario, Playwright must be able to download its driver. `playwright-go` is pinned to v0.5001.0 and `TestMain` overrides `PLAYWRIGHT_DOWNLOAD_HOST` to the live Microsoft CDN automatically (see ADR-0001). If that CDN is also unreachable, the OAuth2 test **skips** (never fails).
3. Run `make test-integration` (brings the stack up, runs the suite, tears it down) or `go test -tags integration -v -timeout 300s ./internal/mcp/...`.

Graceful degradation is intentional: without Docker, all service-dependent tests skip and the stdio test still passes; without a Playwright driver, the OAuth2 test skips. No configuration makes the suite hang or hard-fail on missing infra.

## Findings / decisions

- **ADR-0001 — Playwright driver CDN.** `playwright-go`'s importable versions (module path `playwright-community/playwright-go`, newest v0.5200.1) default their driver download to the retired `playwright.azureedge.net` CDN, which now 404s on every platform. Only driver 1.50.1 (bundled by v0.5001.0) is still served, on the live Microsoft CDN. The change pins v0.5001.0 and overrides `PLAYWRIGHT_DOWNLOAD_HOST` (test-only: `TestMain` + CI env). See `docs/adrs/0001-playwright-integration-driver-cdn.md`.
- **Zero-production-change browser capture.** The production OAuth flow (`GetAccessToken`) opens the browser via `open`/`xdg-open` and does not expose the authorize URL. The OAuth2 test captures it with a PATH-based `open`/`xdg-open` shim (temp dir prepended to `PATH`) — no production code touched.
- **`server-everything` tool reconciliation.** The current `@modelcontextprotocol/server-everything` exposes `echo` and `get-sum` (not `add`) and takes the SSE transport as a positional arg (`sse`, not `--transport sse`) on default port 3001. The compose command and the no-auth assertions were corrected to match.
- **mock-oauth2 healthcheck.** `ghcr.io/navikt/mock-oauth2-server` is a distroless image (no shell/wget), so an in-container healthcheck can never pass. Readiness is gated host-side (`TestMain`, CI wait loop, Make) instead; the container healthcheck was removed.

## Plan deviations

- The `plan`/`build`/`review` role skills (`superpowers:writing-plans` / `subagent-driven-development` / `requesting-code-review`) were not invocable in this harness, so each degraded to `auto` (self-authored plan, self-executed build with TDD-style verification against live services, self-performed whole-branch review) per the convention's missing-skill rule. Noted in the PR body.

## Follow-ups

- If Playwright's importable release line migrates to the `mxschmitt/playwright-go` module path (which uses the current CDN), revisit the pin + host override (ADR-0001 records the trigger). Not filed as a stub — `auto_capture` is disabled and this is captured in the ADR's Consequences.
