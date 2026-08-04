---
name: browser-open-shim-test-pattern
title: Test browser-opening code with a PATH-prepended open/xdg-open shim — no production changes needed
promotion_state: retained
changes: [8]
created: 2026-08-04
updated: 2026-08-04
topics: [testing, oauth2, browser, integration]
---

Production code that calls `open`/`xdg-open` to launch a browser (e.g. for an OAuth2 authorize URL) cannot be tested by mocking `open` directly — the call happens in the production process, and there is no hook to intercept it without adding test-only surface.

**Pattern:** Write a tiny executable shim named `open` (and/or `xdg-open`) to a temp directory, prepend that directory to `PATH` in the test process, then capture the authorize URL it was called with. No production code changes, no new flag, no `os.Getenv` branch.

```go
shim := filepath.Join(t.TempDir(), "open")
os.WriteFile(shim, []byte("#!/bin/sh\necho \"$1\" > /tmp/open-url\n"), 0755)
t.Setenv("PATH", filepath.Dir(shim)+":"+os.Getenv("PATH"))
// run the production code; read /tmp/open-url to get the authorize URL
```

**How to apply:** Whenever production code must launch a browser and you want to test the resulting authorize URL without touching production — OAuth2 PKCE flows, magic-link redirects, etc. Keep shims in `t.TempDir()` so they are cleaned up automatically.

## War story

(#8, PR #8) — Change 0008 (MCP integration test harness). `GetAccessToken` opens the browser via `open`/`xdg-open` with the authorize URL; the OAuth2 integration test captures the URL with a PATH shim and drives the browser flow with Playwright. Zero production code touched.
