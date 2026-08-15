---
id: 60
slug: wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend
title: "Wander live rentals MCP demo — light up #59's live data backend + wire the rentals server into the concierge app"
status: in-progress
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-15
depends_on: [59]
related: [52, 55, 62]
discovered_from: [59]
adrs: []
spec: docs/superpowers/specs/2026-08-15-wander-live-rentals-mcp-demo-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend
claimed_at: 2026-08-15T19:46:01Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-15-wander-live-rentals-mcp-demo-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-wander-live-rentals-mcp-demo-design.md) |
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
