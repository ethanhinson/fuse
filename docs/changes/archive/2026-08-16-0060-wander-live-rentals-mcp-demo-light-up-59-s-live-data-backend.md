---
id: 60
slug: wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend
title: "Wander live rentals MCP demo — light up #59's live data backend + wire the rentals server into the concierge app"
status: done
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-16
depends_on: [59]
related: [52, 55, 62]
discovered_from: [59]
adrs: [43]
spec: docs/superpowers/specs/2026-08-15-wander-live-rentals-mcp-demo-design.md
plan: docs/superpowers/plans/2026-08-15-wander-live-rentals-mcp-demo-plan.md
results: docs/results/2026-08-15-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend-results.md
trivial: false
auto_groomable:
branch: feat/wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/61
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-15-wander-live-rentals-mcp-demo-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-wander-live-rentals-mcp-demo-design.md) |
| Plan | [2026-08-15-wander-live-rentals-mcp-demo-plan.md](https://github.com/ethanhinson/fuse/blob/feat/wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend/docs/superpowers/plans/2026-08-15-wander-live-rentals-mcp-demo-plan.md) |
| Results | [2026-08-15-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend-results.md](https://github.com/ethanhinson/fuse/blob/feat/wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend/docs/results/2026-08-15-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend-results.md) |
| PR | [#61](https://github.com/ethanhinson/fuse/pull/61) |
| ADRs | [ADR-0043](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0043-example-apps-never-publish-credentials-by-default.md) |
<!-- docket:artifacts:end -->

## Why

Change #59 built a real **rentals MCP server** (`internal/mcpdemo/rentals`) — `search_rentals` /
`favorite_listing` / `list_favorites`, with per-principal favorites isolation (the confused-deputy
boundary) and per-principal token adjudication — as the backend for its identity-acceptance CI lane.
It runs against **canned** data behind a one-method `DataSource` seam so the lane stays hermetic, and
its own comments name **#60** as the change that lights up the live backend.

This change makes it a real, browsable demo: implement the live data source, run the server, and wire
it into the Wander concierge so the browser agent searches and favorites **real** listings *as the
authenticated user* over MCP with #52 identity propagation — a demonstration of MCP identity, not just
a test.

## What changes

Design settled in [the spec](../../superpowers/specs/2026-08-15-wander-live-rentals-mcp-demo-design.md);
proposal-altitude summary:

- **Serve the rentals server** via a new thin `cmd/` entrypoint that starts its **existing** HTTP/SSE
  handler (the server is already HTTP-native; it just lacked an entrypoint). The demo declares it as a
  streamable-HTTP MCP server (ADR-0011) so `fuse loop-serve-net` attaches and calls route through the
  #52 identity seam.
- **Live `search_rentals`** implements #59's `DataSource.Search` via Tavily, **reusing the existing
  `TAVILY_API_KEY`**. `CannedData` stays the CI-lane default (acceptance lane untouched, hermetic).
- **Consolidate the two demos into one.** `examples/wander` uses the real `@fuse/sdk` + Connect
  `fuse.loop.v1` wire and carries the reconnect CI lane; `examples/concierge-demo` has the better UI
  but sits on the **pre-#55 WS-proxy wire ADR-0033 removed**. Port concierge-demo's look & feel onto
  wander's SDK/Connect base, drop the obsolete WS proxy, keep the CI lane green. (Also unblocks #62.)
- **Durable per-principal favorites.** #59's favorites store is a hardcoded in-memory map; introduce a
  store seam + a durable impl (favorites survive restart, still strictly per-`{tenant,subject}`). The
  in-memory impl stays the CI default.
- **Identity UX:** a simple token/user picker (tokens → `{tenant,subject}` in `loop_server.auth`);
  switching users shows different favorites — the visible payoff of per-principal MCP identity.

> Reconcile note (verified at groom time): the rentals server is already an HTTP/SSE MCP server (not
> in-process-only), and its favorites store is an in-memory map (not a seam) — so serving is "add an
> entrypoint" and durability is "add a store seam + impl". The two example apps are on different
> transports; consolidation ports UI onto the SDK base, never the reverse.

## Out of scope

- **#59's identity-propagation wiring and its CI lane** — consumed, not rebuilt.
- **New MCP protocol surface** — reuses #59's three tools.
- **#62 refresh-to-restore** (durable browser session resume) — separate change; this only provides the
  SDK/Connect base it needs.
- **`web_fetch`/`web_search` egress identity** — #57 (deferred); unrelated to the rentals MCP path.
- **A production rentals-provider abstraction** — the live `DataSource` is Tavily-backed for the demo.

## Reconcile log

### 2026-08-15 — reconcile at claim (implementer)

Re-read the change + [spec](../../superpowers/specs/2026-08-15-wander-live-rentals-mcp-demo-design.md)
against `origin/main` @ `8fdf2e8`, `related` (#52, #55, #62), the merged `depends_on` (#59), and
ADRs 0011 / 0033 / 0036. **The design holds — no scope invalidation.** Every spec claim re-verified
TRUE against the tree. Four refinements fold in:

1. **D1 correction — the rentals server is `httptest`-shaped, not serve-shaped.** `rentals.go`
   registers `/sse` and `/messages` on a real `http.ServeMux`, but `NewServer` then wraps it in
   `httptest.NewServer` and exposes only `URL()`/`Close()`. So D1 is **not** "add an entrypoint that
   `ListenAndServe`s the existing handler" — the mux must first be exposed through a
   production-shaped constructor (e.g. a `Handler() http.Handler` accessor, or a `NewServer` variant
   that does not self-host), with `httptest` retained for #59's acceptance lane. Slightly more
   surgery than the spec assumed, still small and additive; the acceptance lane stays untouched.

2. **D2 — the reusable Tavily client is `internal/research.TavilyProvider`**
   (`NewTavilyProvider(apiKey string)`, `Search(ctx, query string, maxResults int) ([]SearchResult, error)`),
   with `TAVILY_API_KEY` already resolved by `internal/research.Resolve()`. Its signature does **not**
   match #59's `DataSource.Search(query string) []Listing` (no ctx, no error, different result type),
   so the live source is an **adapter** over it — context and error are absorbed at the seam, a
   failed lookup degrading to an empty result rather than breaking the one-method contract.

3. **D1 transport literal.** #59's acceptance test declares the server as
   `Transport: "sse"` with `Auth: {Type: "identity"}` and an `Audience:` — the demo config must match
   that literal, not a `"streamable-http"` spelling. `config.MCPServerConfig` already carries
   `Audience`/`Scopes` for the #52 identity tier.

4. **D3/D5 config path.** Neither example app ships a fuse config today — both rely on
   `~/.fuse/config.yml`/defaults. The consolidated demo therefore needs a **checked-in demo config**
   to declare the rentals MCP server and the `loop_server.auth` user directory D5's picker reads.
   Confirmed: `examples/wander` carries the only CI lane (`browser_test.go`, `//go:build browser`,
   run by `make browser-test` and the `browser-acceptance` job in
   `.github/workflows/integration.yml`); `examples/concierge-demo` has **no** lane, so
   consolidation moves UI onto wander and retires concierge-demo's `server.js` WS proxy with no lane
   loss.

**Open questions settled at build time, per the spec's own deferral:** favorites durability backs
onto the filesystem store family already in-tree (`internal/event/fsstore`) rather than standing up
Postgres for a demo; the in-memory impl stays the test default. Consolidation lands **into
`examples/wander`** (the path CI already references) with `examples/concierge-demo` deleted.

**Auto-capture:** disabled for this repo (`AUTO_CAPTURE_ENABLED=false`) — one adjacent discovery
noted in prose only, not minted: `internal/mcpdemo/rentals.NewServer` binding its own `httptest`
listener is a test-shaped constructor in a package now consumed by production code paths; the
production/test constructor split this change introduces is worth generalizing if other `mcpdemo`
servers follow.
