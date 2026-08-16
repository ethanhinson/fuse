<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0060 — Wander live rentals MCP demo — light up #59's live data backend + wire the rentals server into the concierge app](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0060-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend.md)**
<!-- docket:backlink:end -->

# Wander live rentals MCP demo — implementation plan

Change: **#0060** · Spec: `docs/superpowers/specs/2026-08-15-wander-live-rentals-mcp-demo-design.md`
(on the `docket` metadata branch) · Base: `origin/main` @ `8fdf2e8` · Depends on #59 (done).

> **Plan-role degrade.** The configured plan skill (`superpowers:writing-plans`) is not available in
> this harness, so this plan was authored directly under the convention's plan auto-fallback. The
> artifact and stop-point are unchanged; only the authoring method differs.

## Goal

Turn #59's canned, test-only rentals MCP server into a real, browsable demo: a live Tavily-backed
data source, a servable entrypoint, durable per-principal favorites, and **one** consolidated demo
app on the `@fuse/sdk` + Connect base carrying concierge-demo's UI — with the browser reconnect CI
lane still green and #59's identity acceptance lane untouched.

## Invariants (hold across every task)

1. **#59's acceptance lane is untouched and stays green.** `cmd/fuse/loop_serve_net_rentals_acceptance_test.go`
   must keep compiling and passing without edits to its assertions. `CannedData` stays the default
   `DataSource`; the in-memory favorites store stays the default store.
2. **The confused-deputy boundary never moves.** A favorite always lands in the *calling* token's
   set, keyed by `principalKey{tenant, subject}` derived from the verified token — never from a
   client-supplied argument. Durability changes *where* the set lives, never *whose* set a write
   lands in.
3. **No new MCP protocol surface.** The three tools (`search_rentals`, `favorite_listing`,
   `list_favorites`) and their schemas in `toolDefs()` are unchanged.
4. **Live network is demo-only.** No test added by this plan may require `TAVILY_API_KEY` or reach
   the network; the live path is exercised through a fake HTTP server or a stub provider.

## Learnings that bear on this change (read before the task they annotate)

- **`httptest-defer-close-before-tcleanup-deadlock`** (tasks 1, 3, 5) — the rentals server's SSE
  handler loops on `r.Context().Done()`, so `Close()` blocks until the client read-pump disconnects.
  In any test touching it, register **both** teardowns with `t.Cleanup` (server first, client
  second, so LIFO stops the client first). Never `defer srv.Close()`. This deadlock has already
  bitten twice (#52, #59) as a silent 600s package timeout, not an assertion failure.
- **`cache-over-tenant-scoped-source-reassert-key-on-hit`** (tasks 2, 3) — favorites are
  key-partitioned by the compound `{tenant, subject}`. Any in-memory cache layered over the durable
  store must key on (or re-assert) the **full** compound key on every hit; a hit keyed by subject
  alone crosses tenants. Prefer keying the map by the whole `principalKey`.
- **`fanout-send-snapshot-identity-not-pointer`** (task 1) — `handleSSE` appends channels to
  `s.sseConns`. If task 1 touches the fan-out or adds a shutdown path, carry **identity** across the
  lock boundary and re-validate membership under the re-acquired lock before each non-blocking send;
  never send on a pointer snapshotted outside the lock.
- **`scripted-gateway-double-double-escapes-tool-args`** (task 9, if a scripted lane is touched) —
  pass **plain** JSON tool-call args at call sites into an `LLM_GATEWAY_URL` double; pre-escaped args
  double-escape and hang the loop with no failing assertion.
- **`connect-stream-terminal-errors-stop-reconnect`** / **`abortable-client-stream-teardown`**
  (tasks 7, 8) — the consolidated demo keeps wander's SDK reconnect semantics: terminal Connect codes
  must close and surface, and the observe stream needs its idempotent abort teardown. Do not
  regress these while porting UI.

---

## Task 1 — Production-shaped constructor for the rentals server

**Why:** the spec assumed the server only lacked an entrypoint. It actually self-hosts: `NewServer`
builds a real `http.ServeMux` and then wraps it in `httptest.NewServer`, exposing only `URL()` /
`Close()` (`internal/mcpdemo/rentals/rentals.go`, `NewServer`). Nothing can serve it on a real port.

**Do:**
- Extract mux construction into an unexported `newMux()` (or equivalent) used by both paths.
- Add an exported `Handler() http.Handler` (or `NewHandler(cfg Config) (*Server, http.Handler)`)
  returning the routed mux **without** binding a listener.
- Keep `NewServer(cfg)` behaviourally identical — same signature, still `httptest`-backed, still
  `URL()`/`Close()` — so #59's acceptance test and `rentals_test.go` need no edits.
- Update the `NewServer` doc comment: it is the **test** constructor; `Handler()` is the servable one.

**Test first:** a test that builds the handler via the new accessor, serves it on a caller-owned
`httptest.NewServer(h)`, and drives one `tools/list` + one authorized `search_rentals` over it —
proving the handler is complete without `NewServer`'s self-hosting. Teardown via `t.Cleanup` only.

**Done when:** `go test ./internal/mcpdemo/... ./cmd/fuse/...` is green with zero edits to
`loop_serve_net_rentals_acceptance_test.go`.

**Profile:** standard (a behaviour-preserving extraction with a live-goroutine teardown hazard).

---

## Task 2 — Favorites store seam + in-memory implementation (behaviour-preserving)

**Why:** favorites are a hardcoded `map[principalKey]map[string]bool` under `s.mu`
(`addFavorite`, `listFavorites`). D4 needs a seam before durability can be swapped in.

**Do:**
- Define a small exported interface in the `rentals` package, e.g.
  `type FavoritesStore interface { Add(pk PrincipalKey, listingID string) error; List(pk PrincipalKey) ([]string, error) }`.
- Export `PrincipalKey{Tenant event.TenantID; Subject string}` (or keep it unexported and give the
  store package-internal visibility) — whichever keeps the compound key intact. **The key must stay
  compound**; see the tenant-reassert learning.
- Provide `NewMemoryFavorites()` implementing it with today's map+mutex semantics (idempotent add).
- Add `Config.Favorites FavoritesStore`; `nil` ⇒ `NewMemoryFavorites()` (the CI default).
- Rewrite `addFavorite`/`listFavorites` as thin delegations. Tool handlers keep deriving `pk` from
  the **verified token**, never from args.

**Test first:** table test over the interface (seeded via `Config.Favorites`) asserting: add is
idempotent; two subjects in one tenant do not see each other's favorites; the **same subject string
under two different tenants** does not either (the compound-key regression).

**Done when:** the suite is green and the acceptance lane still passes unmodified.

**Profile:** standard.

---

## Task 3 — Durable filesystem favorites implementation

**Why:** D4's pinned contract — favorites survive a rentals-server restart, still strictly
per-principal. The reconcile settled the open question in favour of the filesystem store family
already in-tree (`internal/event/fsstore`) rather than standing up Postgres for a demo.

**Do:**
- Implement a durable `FavoritesStore` persisting under a caller-supplied root directory, one
  file/key per `PrincipalKey`. Encode the tenant and subject in the path safely — **escape or hash
  them**; a raw subject containing `/` or `..` must not escape the root (add a test).
- Writes must be atomic (temp file + rename) and concurrent-safe (mutex around read-modify-write).
- Wire it as a `cmd/rentals-mcp` flag/env option only (task 5). It is **never** the test default.

**Test first:** create store at a `t.TempDir()`, add favorites for two principals, construct a
**second** store instance over the same root (the restart), assert both principals' sets come back
exactly and remain disjoint. Plus a path-traversal test for hostile subjects.

**Done when:** `go test ./internal/mcpdemo/...` green, including under `-race`.

**Profile:** standard.

---

## Task 4 — Live Tavily-backed `DataSource`

**Why:** D2. The reusable client is `internal/research.TavilyProvider` —
`NewTavilyProvider(apiKey string)` with
`Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)`. It does **not**
match `DataSource.Search(query string) []Listing`, so this is an adapter, not a drop-in.

**Do:**
- Add a live data source that holds a searcher, a context, and a result cap, and implements
  `Search(query string) []Listing` by calling the provider and mapping `SearchResult` → `Listing`
  (`ID`, `Title`, `City`, `Price`).
- Depend on a **narrow local interface** (`interface{ Search(ctx, string, int) ([]SearchResult, error) }`),
  not the concrete `*TavilyProvider`, so the test can inject a stub without network.
- Absorb ctx and error at the seam: on error or ctx cancellation return an **empty slice**, never
  panic — the one-method contract has no error channel. Log/record the failure.
- Deterministic ID derivation (e.g. stable hash of the result URL) so `favorite_listing` on a search
  result is meaningful across calls. Price/city are best-effort extraction from the result; document
  that they are demo-grade.
- Bound the call: apply a per-search timeout (see the `bound-every-model-call` discipline).

**Test first:** stub searcher returning fixed results → assert mapping, stable IDs, cap respected;
stub returning an error → assert empty slice, no panic; stub that blocks → assert the timeout
returns empty. **No network in any test.**

**Done when:** package tests green; `CannedData` still the default.

**Profile:** standard.

---

## Task 5 — `cmd/rentals-mcp` entrypoint

**Why:** D1 — something must actually serve the handler from task 1.

**Do:**
- New `cmd/rentals-mcp/main.go`: flags/env for listen address, `--audience`, tenant signing keys,
  favorites directory (empty ⇒ in-memory), and the data source selector (canned vs. Tavily via
  `TAVILY_API_KEY`, resolved through `internal/research.Resolve()` — reuse, do not re-read the env
  ad hoc).
- Build `Config`, get `Handler()`, `http.Server.ListenAndServe` with read/write timeouts and
  graceful shutdown on SIGINT/SIGTERM.
- Fail loud with a clear message on missing audience or unparseable tenant keys.
- Short README section documenting the flags.

**Test first:** a test that starts `main`'s wiring function (factor the wiring into a testable
`run(args, env) error`, not inline in `main`) on `127.0.0.1:0`, hits `/sse`, asserts the endpoint
event arrives, then shuts down cleanly — all teardown via `t.Cleanup`.

**Done when:** `go build ./...` and `go test ./cmd/...` green.

**Profile:** standard.

---

## Task 6 — Demo fuse config declaring the rentals MCP server + auth directory

**Why:** neither example ships a config today; both rely on `~/.fuse/config.yml`. D1 and D5 both
need a checked-in one.

**Do:**
- Add a demo config under the consolidated example declaring the rentals server with the **literal
  shape #59's acceptance test uses** — `transport: sse`, `url:`, `audience:`, `auth: {type: identity}`
  (`config.MCPServerConfig` / `MCPAuthConfig` in `internal/config/schema.go`). Do **not** invent a
  `streamable-http` spelling.
- Declare a `loop_server.auth` user directory mapping at least two demo bearer tokens to distinct
  `{tenant, subject}` pairs — this is D5's user directory.
- Demo tokens are obviously-fake, checked-in, documented as demo-only.

**Test first:** a config-parse test asserting the checked-in demo config loads and yields the
expected MCP server entry (identity tier + audience) and ≥2 distinct principals — so the demo config
can never silently rot away from the schema.

**Done when:** config test green.

**Profile:** standard.

---

## Task 7 — Consolidate the demos: port concierge-demo's UI onto `examples/wander`

**Why:** D3. `examples/wander` has the SDK + Connect base and the **only** CI lane
(`browser_test.go` `//go:build browser`, `make browser-test`, `browser-acceptance` job in
`.github/workflows/integration.yml`). `examples/concierge-demo` has the better UI and a dead
pre-#55 `/ws` + `GET /loops/{id}/events` proxy in its `server.js`, and no lane.

**Do:**
- Port `examples/concierge-demo`'s `styles.css` and `index.html` layout/look onto
  `examples/wander`, keeping wander's `app.js` SDK wiring (`createClient`, `startLoop`, `send`,
  `observe`, `isCompletion`, `FuseTerminalError`) as the transport. **UI moves onto the SDK base;
  the transport never moves backwards.**
- Keep wander's `server.js` static/reverse-proxy role and `build.sh` esbuild bundling intact.
- **Delete `examples/concierge-demo` entirely** (including its WS-proxy `server.js`), and update any
  README/doc references to point at `examples/wander`. Carry over its `screenshot.png` if the README
  uses one.
- Update `examples/wander/README.md` to describe the consolidated demo.

**Test first / gate:** `make browser-test` must pass. Update `browser_test.go` selectors **only**
where the ported markup renamed them — do not weaken assertions, and do not remove the reconnect
scenario.

**Done when:** `go test -tags browser -timeout 300s ./examples/wander/...` green and no reference to
`examples/concierge-demo` remains (`grep -r concierge-demo` clean apart from historical docs).

**Profile:** premium — this is the task that can silently break the repo's only browser acceptance
lane, and "port the UI" is where an agent is most tempted to relax a selector assertion.

---

## Task 8 — Identity UX: token/user picker + favorites view

**Why:** D5 — the visible payoff. Switching users must show a **different** favorites list.

**Do:**
- Add a lightweight picker to the consolidated UI: choose among the demo tokens from task 6 (or
  paste one). The chosen token is sent as the bearer credential on the SDK client, so
  `loop_server.auth` resolves `{tenant, subject}` and the identity tier mints the audience-bound
  delegation token the rentals server adjudicates.
- Add a "Saved" panel populated by calling `list_favorites` through the agent, refreshed after a
  favorite lands and on user switch.
- Switching user must **fully reset** client-side state (abort the observe stream via the SDK's
  idempotent teardown, clear the transcript and the saved panel) before starting the new session —
  a stale stream from the previous principal must not leak into the new one.

**Test first:** extend the browser lane with a scenario that selects user A, favorites a listing,
switches to user B, and asserts B's saved panel does **not** contain A's listing. If driving two
real principals end-to-end proves too heavy for the lane, cover the reset/teardown logic in a
JS-level or Go-level unit test and document the gap — do not silently skip.

**Done when:** browser lane green including the new scenario.

**Profile:** premium — this is the change's security-visible claim (per-principal isolation shown in
a UI); a bug here demos the opposite of the point.

---

## Task 9 — Full-suite gate + docs

**Do:**
- `make test` (unit) and `make test-race`, then `make browser-test`.
- If any lane was touched, re-run it explicitly rather than trusting the aggregate.
- Update `examples/wander/README.md` + `internal/mcpdemo/rentals` doc comments so the "#60 lights
  this up" comments now describe the shipped state rather than a promise.

**Done when:** unit, race, and browser lanes are all green on the branch, and the build-evidence
record is minted at the branch HEAD.

**Profile:** standard.

---

## Out of scope (do not do)

- Rebuilding #59's identity propagation or its acceptance lane.
- Any new MCP tool or schema change.
- #62 refresh-to-restore (durable browser session resume).
- `web_fetch` / `web_search` egress identity (#57, deferred).
- A production, provider-agnostic rentals abstraction — Tavily-backed is the demo contract.
- Postgres for favorites — the filesystem impl is the settled choice.
