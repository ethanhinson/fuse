<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0060 — Wander live rentals MCP demo — light up #59's live data backend + wire the rentals server into the concierge app](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0060-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend.md)**
<!-- docket:backlink:end -->

# Wander live rentals MCP demo — design

Change: [#0060](../../changes/active/0060-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend.md) ·
Depends on #59 (done) · Related #52, #55, #62 · ADRs consulted: 0011 (streamable-HTTP MCP), 0033 (Connect wire), 0036 (tool identity)

## Problem

Change #59 built a real **rentals MCP server** (`internal/mcpdemo/rentals`) — `search_rentals`,
`favorite_listing`, `list_favorites`, with per-principal favorites isolation (the confused-deputy
boundary: a favorite always lands in the *calling* token's set, keyed by `principalKey{tenant,
subject}`, never a client-supplied arg) and per-principal token adjudication (audience-bound,
verified under the tenant signing key). It runs against **canned data** behind a one-method
`DataSource` seam (`Search(query string) []Listing`) so its CI acceptance lane stays hermetic. The
server's own comments name **#60** as the change that lights up the live backend.

This change makes it a real, browsable demo: implement the live data source, run the server, and wire
it into the Wander concierge so the browser agent searches and favorites **real** listings *as the
authenticated user* over MCP with #52 identity propagation — a demonstration of MCP identity, not
just a test.

## Reconciliation notes (verified against the tree at groom time)

Two stub/PM-altitude assumptions were corrected by reading the code; the spec is written against
reality:

1. **The rentals server is ALREADY an HTTP/SSE MCP server**, not an in-process-only object
   (`rentals.go`: `mux.HandleFunc("/sse", s.handleSSE)`, "the rentals MCP HTTP/SSE server"). What is
   missing is a **`cmd/` entrypoint that starts it listening** — today it is only constructed inside
   `cmd/fuse/loop_serve_net_rentals_acceptance_test.go`. So serving it is "add an entrypoint + config
   declaration", not "add a transport".
2. **The favorites store is a hardcoded in-memory `map[principalKey]map[string]bool` under a mutex**
   (`rentals.go` `favs`, `addFavorite`, `listFavorites`) — NOT a store seam. "Real durability" (D4)
   therefore requires introducing a store interface and a durable implementation, then swapping the
   map behind it.

Also: **the two example apps are on different transports.** `examples/wander` uses the real published
`@fuse/sdk` bundle over the **Connect `fuse.loop.v1`** wire (ADR-0033) and carries the permanent
headless-browser reconnect CI lane (`browser_test.go`). `examples/concierge-demo` uses a **hand-rolled
WebSocket proxy** (`server.js`) over the **pre-#55 `/ws` + `GET /loops/{id}/events` wire that ADR-0033
replaced and #55 removed** — a dead transport — with no SDK and no CI lane, but a better UI. The
consolidation (D3) therefore ports the UI onto the SDK base, never the reverse.

## Decisions

### D1 — Serve the rentals server via a new `cmd/` entrypoint (streamable-HTTP MCP)

Add a thin `cmd/rentals-mcp` (name at build discretion) that constructs `rentals.NewServer(cfg)` and
`ListenAndServe`s its **existing** HTTP/SSE handler on a local port. The consolidated demo declares it
as a streamable-HTTP MCP server (ADR-0011) in the demo's fuse config, so `fuse loop-serve-net` attaches
to it and every `search_rentals`/`favorite_listing`/`list_favorites` call routes through the #52
identity seam (the loop initiator's token reaches the rentals server, which adjudicates audience +
per-principal favorites). No stdio transport is added — the server is HTTP-native.

### D2 — Live `search_rentals` backend implements `DataSource.Search` via Tavily, reusing the demo key

Implement a live `DataSource` (`Search(query string) []Listing`) that queries a Tavily-style rentals
lookup and maps results to `[]Listing`, swapped in for the demo (`CannedData` stays the CI-lane
default — the acceptance lane is untouched and stays hermetic). It **reuses the existing
`TAVILY_API_KEY`** the demo `run.sh` already pulls for `web_search` — one credential, one config path.
Live network is confined to the demo; the seam boundary (D2 is a single method) keeps this small.

### D3 — Consolidate the two demos: port concierge-demo's UI onto wander's SDK/Connect base

Produce **one** demo. Keep `examples/wander`'s foundation — the real `@fuse/sdk` bundle, the Connect
`fuse.loop.v1` transport, and the headless-browser reconnect CI lane — and bring
`examples/concierge-demo`'s superior look & feel (its `styles.css`, `index.html` layout, and UX) onto
it. **Drop concierge-demo's obsolete WS-proxy `server.js`** and its pre-#55 wire entirely. Exact final
directory layout (consolidate into `examples/wander`, or a new merged dir, and how the old paths are
retired/redirected) is a build-time choice; the invariant is: **one demo app, concierge-demo's UI,
wander's SDK + Connect transport, the reconnect CI lane preserved and still green.** This also unblocks
#62 (refresh-to-restore needs the SDK's `observe(loopId, {fromSeq})`, which only the wander base has).

### D4 — Durable per-principal favorites

Introduce a favorites-store seam in the rentals server (a small interface: get/add/list per
`principalKey`) and a durable implementation backing it, replacing the hardcoded in-memory map. The
per-principal key stays the token identity (`{tenant, subject}`) — the confused-deputy boundary is
preserved exactly; durability changes *where* the set is stored, never *whose* set a write lands in.
`CannedData`/CI-lane behavior stays green: the in-memory impl remains available (and is the test
default) so the acceptance lane needs no durable store. Store technology (reuse the project's existing
durable store from #47, or a simple embedded KV) is a build-time choice; the pinned contract is
**favorites survive a rentals-server restart, still strictly per-principal**.

### D5 — Identity UX: a simple token/user picker

The consolidated UI gets a lightweight login: pick or enter a bearer token, each mapped to a
`{tenant, subject}` in the demo's `loop_server.auth` config. Switching users shows a **different**
favorites list — the visible payoff of per-principal MCP identity ("favorite as the authenticated
user"). No real IdP; the demo's auth config is the user directory.

## What changes (scope)

- `internal/mcpdemo/rentals`: live `DataSource` (Tavily) + a favorites-store seam with a durable impl
  (in-memory retained as the CI default).
- `cmd/rentals-mcp` (new): thin entrypoint that serves the existing HTTP/SSE handler.
- `examples/` consolidation: one demo on the `@fuse/sdk` + Connect base with concierge-demo's UI;
  declares the rentals MCP server + reuses `TAVILY_API_KEY`; token/user picker; favorites "saved" view
  backed by `list_favorites`; obsolete WS-proxy removed.
- The headless-browser reconnect CI lane is carried onto the consolidated demo and stays green.

## Out of scope

- **#59's identity-propagation wiring and its CI lane** — consumed, not rebuilt.
- **New MCP protocol surface** — reuses #59's three tools.
- **#62 refresh-to-restore** (durable session resume in the browser) — separate change; this only
  provides the SDK/Connect base it needs.
- **`web_fetch`/`web_search` egress identity** — #57 (deferred); unrelated to the rentals MCP path.
- **A production rentals provider abstraction** — the live `DataSource` is Tavily-backed for the demo;
  a provider-agnostic config is not required.

## Open questions for the reconcile pass

- Final consolidated demo directory layout and how the retired `examples/concierge-demo` path is
  handled (delete vs. redirect) — settle at build time; the CI lane wiring (`browser_test.go`,
  `build.sh`) must move with it and stay green.
- Which durable store backs favorites (reuse #47's durable store vs. an embedded KV) — pick at build
  time against the pinned "survives restart, per-principal" contract.
