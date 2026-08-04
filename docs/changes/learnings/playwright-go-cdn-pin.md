---
name: playwright-go-cdn-pin
title: playwright-go CDN is retired — pin v0.5001.0 and override PLAYWRIGHT_DOWNLOAD_HOST
promotion_state: candidate
changes: [8]
created: 2026-08-04
updated: 2026-08-04
topics: [testing, playwright, docker, ci, integration]
---

`playwright-go`'s importable release line (`playwright-community/playwright-go`) defaults driver downloads to `playwright.azureedge.net`, which now 404s on every platform. Only driver version 1.50.1 (bundled by module v0.5001.0) is still served, on the live Microsoft CDN (`playwright-cdn.azureedge.net`).

**Rule:** When adding `playwright-go` to any package, pin to `playwright-community/playwright-go@v0.5001.0` and set `PLAYWRIGHT_DOWNLOAD_HOST` to the live Microsoft CDN in `TestMain` and CI env. Without the pin and override, driver download silently fails and every Playwright test skips.

**How to apply:** In `TestMain`:
```go
os.Setenv("PLAYWRIGHT_DOWNLOAD_HOST", "https://playwright.azureedge.net") // wrong — retired
// Use instead:
os.Setenv("PLAYWRIGHT_DOWNLOAD_HOST", "https://playwright-cdn.azureedge.net")
```
Set the same env var in CI. Watch for the `mxschmitt/playwright-go` module path — when it becomes importable with a current CDN, the pin + override can be lifted (ADR-0001 records the trigger).

## War story

(#8, PR #8) — Change 0008 (MCP integration test harness). The `playwright-go` module's retired CDN caused driver downloads to 404 on CI. Only after pinning v0.5001.0 and overriding `PLAYWRIGHT_DOWNLOAD_HOST` did the OAuth2 browser-flow test pass reliably. See ADR-0001.
