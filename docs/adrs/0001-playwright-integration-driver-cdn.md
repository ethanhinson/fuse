---
id: 1
slug: playwright-integration-driver-cdn
title: "Playwright integration tests: pin playwright-go and override the driver CDN"
status: Accepted
date: 2026-08-04
supersedes: []
reverses: []
relates_to: []
change: 8
---

## Context

Change 0008 adds `//go:build integration` tests in `internal/mcp/`, one of which (`TestIntegration_HTTP_OAuth2`) drives the full OAuth2 authorization-code + PKCE flow through a real headless Chromium browser via `github.com/playwright-community/playwright-go`, against the `ghcr.io/navikt/mock-oauth2-server` container.

Two hard constraints were discovered during the build:

1. **Driver-CDN retirement.** The canonical importable module path is `github.com/playwright-community/playwright-go`. Every version whose go.mod declares that path (the highest importable is v0.5200.1) downloads its Playwright driver from the retired `playwright.azureedge.net` CDN, which now returns 404 for the driver zip on every platform (mac-arm64 and linux alike). Newer releases (v0.5700.0+) declare the module path `github.com/mxschmitt/playwright-go` and therefore cannot be imported as `playwright-community/...`. So the newest usable version is effectively capped, and its default CDN is dead.

2. **The production OAuth flow opens the browser itself.** `mcp.GetAccessToken` (via `runBrowserFlow`/`openBrowser`) shells out to `open`/`xdg-open` with the authorize URL and does not return that URL to the caller. Change 0008's out-of-scope rule forbids any production-code change.

## Decision

- Pin `github.com/playwright-community/playwright-go` to **v0.5001.0** — the version whose bundled driver (Playwright 1.50.1) is the one driver version still hosted on the live Microsoft CDN (`https://playwright.download.prss.microsoft.com/dbazure/download/playwright`), confirmed by probing the CDN across driver versions (only 1.50.1 returns 200).
- Override the driver download host by setting `PLAYWRIGHT_DOWNLOAD_HOST` to that Microsoft CDN. It is set in two places, both test-only: `TestMain` sets it (only if unset) before `playwright.Install`, and the CI workflow sets it as a job env var. No production code is touched.
- Observe the production browser-open **without changing production code** via a PATH shim: the test prepends a temp dir to `PATH` containing an `open`/`xdg-open` script that writes its URL argument to a file; the test polls that file and drives Playwright to the captured authorize URL. mock-oauth2 runs with `interactiveLogin: false` so the authorize endpoint auto-redirects to the local callback with a code.
- Every Playwright/Docker-dependent test **skips gracefully** (never fails) when the driver or services are unavailable, so the default `go test ./...` build and CI-less local runs are unaffected.

## Consequences

- Enables a genuine end-to-end OAuth2 browser test (verified passing locally: full authorize→callback→token-exchange→tools/list plus silent refresh) with zero production-code change.
- Couples the test suite to a specific playwright-go version and an external CDN URL that Microsoft could also retire; if that happens the OAuth2 test degrades to a skip rather than a hard failure, and the pin/host must be revisited (e.g. migrating to the `mxschmitt/playwright-go` import path once the project standardizes on it).
- The in-process OAuth proxy performs only coarse Bearer-presence validation (not full JWKS signature verification); the real signature is produced by mock-oauth2, and proving the client acquired and attached that token is the harness's goal.
