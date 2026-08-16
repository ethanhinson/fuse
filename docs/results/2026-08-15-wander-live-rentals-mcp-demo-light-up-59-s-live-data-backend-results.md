<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0060 — Wander live rentals MCP demo — light up #59's live data backend + wire the rentals server into the concierge app](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0060-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend.md)**
<!-- docket:backlink:end -->

# Wander live rentals MCP demo — results

Change: #0060 · Branch: `feat/wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend` ·
PR: (opened at close-out) · Plan: `docs/superpowers/plans/2026-08-15-wander-live-rentals-mcp-demo-plan.md` ·
ADRs: 0043

## Verify (human)

The automated lanes cover the wire, the isolation boundary, and the reconnect path. What they cannot
reach is whether the demo is actually *demoable* — this change's whole point is a browsable artifact,
and the browser lanes drive it headlessly with canned data.

- [ ] **Run the demo end to end from the documented path.** `cp examples/wander/fuse.demo.yml ~/.fuse/config.yml`,
      then `./examples/wander/run.sh`, then open `http://127.0.0.1:5173`. Confirm all three processes
      come up and the page loads. (The documented run path was dead on arrival until fix `7a91bf7` —
      worth confirming on your machine, since the CI lane starts the binaries itself and would not
      have caught a second regression here.)
- [ ] **Confirm the live backend actually returns real listings.** With `TAVILY_API_KEY` set, ask the
      concierge for rentals somewhere real. No automated test exercises the live provider — every test
      uses a stub, by design, so the Tavily mapping (title/city/price extraction) has never run against
      a real response. Check that the cards are not obviously garbage: city and price extraction are
      regex/substring best-effort and explicitly demo-grade.
- [ ] **Judge the ported UI by eye.** The concierge-demo look was ported onto the SDK base and the
      original is deleted; the browser lane asserts behavior, not appearance. Two deliberate visual
      deviations to sanity-check: the remote webfont link was dropped (system-font fallback) so the
      lane stays hermetic, and the send button ships without a markup-level `disabled`.
- [ ] **Switch users in the browser and watch the Saved panel change.** This is the change's headline
      claim. The isolation itself is asserted by `TestWanderBrowserUserSwitchIsolatesFavorites`, but
      whether the *payoff is legible to a viewer* is a judgement call only you can make.
- [ ] **Decide whether the demo user directory should ship in the repo at all.** `fuse.demo.yml`
      carries obviously-fake bearer tokens and `server.js` will publish them on `/demo-users.json`
      (loopback-only, and fail-closed for any other config — see ADR-0043). The posture is sound; the
      product question of shipping any token-shaped string in the repo is yours.

### Interactive verification outcomes (2026-08-16, at finalize)

Run by the maintainer before merge. `TAVILY_API_KEY` sourced from `~/dev/llm-research-agent/.env`;
loop backend on the local LiteLLM gateway (`glm` / cloud/glm-5.2), never Claude.

- [x] **Live backend returns real listings — CONFIRMED.** Ran the production path
      (`research.Resolve` → `NewLiveData` → `Search`) against the real Tavily API for three rental
      queries. All returned 5 results, mapped to `Listing` cards with clean relevant titles and stable
      deterministic `live-<hash>` IDs. The Tavily→card mapping works. City/price extraction behaved
      exactly as documented (best-effort): city often empty or a ragged trailing segment
      (`"Austin, TX - 334 Rentals | Trulia"`), price usually `0`, with occasional over-eager hits
      (a Lake Tahoe result mapped `$7000` — almost certainly a property value, not a nightly rate) and
      one non-listing result (`"Instagram"`). None of this is a bug; it is the demo-grade contract.
- [x] **Demo comes up end to end from the documented path — CONFIRMED.** `run.sh` built the SDK
      bundle + both binaries and stood up all three processes (rentals-mcp, loop-serve-net, static
      page); `data=*rentals.LiveData provider=tavily`, page HTTP 200, 2 principals in the picker,
      SSE endpoint reachable. (Ran on port 5273 rather than the default 5173, which was occupied by an
      unrelated process on this machine.)
- [x] **Observability wired end to end — CONFIRMED.** Enabled the `observability:` block against the
      already-running `deploy/observability` compose stack. Loop `/metrics` served `fuse_` metrics on
      :9090; Prometheus target `fuse` = up; traces exported through the OTLP collector into Tempo
      (20+ `fuse.*` traces, incl. `fuse.api.request.start_loop` roots, queryable via
      `{resource.service.name="fuse"}`). Two Grafana-UI gotchas noted, neither a fuse defect: the
      Traces-Drilldown "Span rate" view needs a Tempo metrics-generator this dev stack does not
      configure (`empty ring`), and the visual Search builder emits unquoted TraceQL that this Tempo
      rejects — type the quoted query in the TraceQL tab instead.

Not separately re-verified here (asserted by the automated lanes and/or a pure judgement call left to
the maintainer): the ported UI look, the browser user-switch Saved-panel payoff, and the
ship-the-demo-user-directory product question.

## Findings

The whole-branch review (deep rung) returned **8 findings — 1 blocker, 3 important, 4 minor**. All 8
were fixed in-branch before the PR opened; the suite went green on the first post-fix gate run, so
nothing was reverted. Full dispositions are in the PR body. The three worth reading here:

- **The blocker was a real isolation defect, not a style point** (`6c99aa9`). The rentals MCP server
  broadcast every JSON-RPC response to *every* connected SSE client and never pruned dead
  connections. Latent while the server was test-only (one client), but this change made it a
  long-lived process that `cmd/fuse` opens a **per-loop** connection to — and `internal/mcp`'s client
  numbers request ids per-client, so two principals' id spaces collide by construction. One
  principal's client could resolve its pending call with **another principal's favorites** — the exact
  opposite of what the demo exists to show. Fixed by session-scoping: each SSE stream gets an id
  advertised on the endpoint event, responses route only to the owning session, and channels are
  pruned on disconnect. Delivery now fails closed (an unknown session delivers nowhere).
  Notably, **the browser lane passed the whole time** — it switches users sequentially, so the stray
  frames had no pending id to corrupt and were silently dropped. The leak was on the wire with nothing
  asserting against it.
- **`_default` principals could never authenticate** (`32f823d`). `cmd/fuse` seeds `event.DefaultTenant`
  with the *raw* signing key while `cmd/rentals-mcp` derived one for every tenant name. Since the
  picker's first and default option is `{token: "fuse-dev-token", tenant: "_default"}`, the
  zero-config path the README advertises was broken. The derivation *functions* were byte-identical —
  the divergence was in the callers, which is the harder kind to notice. There is now a golden-vector
  parity test in each package (`9699e71`) so drift fails in `make test` rather than only in the
  `browser`-tagged lane.
- **A credential surface became an ADR** (`9013766`, **ADR-0043**). The token endpoint added for the
  picker was guarded only by a `console.warn`, on a Node server that bound all interfaces. The bind
  was pre-existing from #0056; this change is what turned it into an exposure. That generalizes —
  a demo server's existing network posture becomes a real exposure the moment a later change teaches
  it to serve secrets — so it was recorded as policy rather than patched quietly.

Two defects were also caught by build workers rather than the reviewer, both of which would have made
the demo non-functional: the demo config declared `url: …:8091/sse` while `internal/mcp/http_client.go`
appends `/sse` itself (so it dialed `/sse/sse`), and `run.sh` had a multibyte `…` absorbed into a
variable name, which aborted the script under `set -u`.

### Plan deviations

- **Task 1 was larger than the spec assumed.** The spec said the rentals server "just lacked an
  entrypoint". It actually self-hosted via `httptest.NewServer`, so a listener-free constructor had to
  be extracted first. Caught at reconcile and recorded there before planning.
- **`WriteTimeout` deliberately left unset** in `cmd/rentals-mcp`. A write deadline severs every live
  SSE session at exactly that interval; `ReadHeaderTimeout`/`ReadTimeout`/`IdleTimeout` are set and
  shutdown bounds the stream instead.
- **`deriveTenantKey` is duplicated** between `cmd/fuse` and `cmd/rentals-mcp` (both `package main`,
  so it cannot be imported). Not refactored here — the parity is now test-guarded instead. See
  follow-ups.

## Follow-ups

Auto-capture is disabled for this repo, so none of these were minted as stubs — they are recorded here
for your judgement:

- **Hoist `deriveTenantKey` into an internal package.** Two `package main` copies kept in sync by
  golden-vector tests is a guard, not a fix. The real remedy is a shared `internal/` home.
- **`cmd/fuse/mcp_stub_server_test.go` carries the same broadcast-to-all-connections shape** the
  blocker fixed in the real server. It is a single-client test stub so it is not a live risk, but it
  is the same defect waiting for anyone who reuses it for a multi-client case.
- **The durable favorites store is atomic but not crash-durable** (the containing directory is not
  fsynced after rename) and serializes writers within one process only. Both are documented on the
  type and acceptable for a demo; they would not be for a real store.
- **`examples/wander/server_exposure_test.go` lives under the `browser` build tag**, so the
  credential-exposure guards do not run in the default `go test ./...`. It needs no browser — moving
  it to an untagged lane would widen the coverage cheaply.
- **The live search path has no automated coverage against a real provider.** Every test stubs the
  searcher (deliberately — no network in tests), which means the Tavily response mapping is only ever
  verified by a human running the demo.
