---
id: 60
slug: wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend
title: "Wander live rentals MCP demo — light up #59's live data backend + wire the rentals server into the concierge app"
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: []
related: []
discovered_from: [59]
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

Change #59 builds a real **rentals MCP server** (per-principal favorites store + `search_rentals` /
`favorite_listing` / `list_favorites`) as the backend for its permanent identity-acceptance CI lane,
running against a **canned/deterministic** data source so the lane stays hermetic. The server is
designed with a **data-source seam**: `search_rentals` can be backed by real rental lookups
(Tavily-style) instead of canned data.

This change lights up that live backend and wires the rentals MCP server into the **runnable Wander
demo** (`examples/concierge-demo`), so the concierge can actually favorite real listings *as the
authenticated user* over MCP with #52 identity — a real, browsable end-to-end demo of MCP identity
propagation, not just a test.

## What changes

- Implement the live (Tavily-style) `search_rentals` data-source backend behind #59's seam.
- Wire the rentals MCP server into `examples/concierge-demo` (`run.sh` + config) so Wander runs it
  alongside `fuse loop-serve-net`, and the browser concierge can search + favorite listings scoped to
  the logged-in principal.
- Surface favorites in the Wander UI (a "saved" view backed by `list_favorites`).

## Out of scope

- The identity-propagation wiring and the permanent CI lane themselves — that is #59 (this consumes
  the server #59 ships).
- New MCP protocol surface — reuses #59's server tools.

## Open questions

- Whether the live rentals backend reuses Wander's existing `web_search`/Tavily config or gets its own.
- Persistence: does the demo favorites store stay in-memory (per #59) or gain real durability.
- How the per-principal identity surfaces in the demo UI (login/token UX).
