<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0048 — Networked binding over the Runtime seam — WS live observe + HTTP start/send/replay](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0048-networked-runtime-binding.md)**
<!-- docket:backlink:end -->

# Networked binding over the Runtime seam — implementation plan

> **For agentic workers:** Implement this plan task-by-task with TDD (a focused failing test first, then the code to green it, then refactor). Steps use checkbox (`- [ ]`) syntax for tracking. Each task ends green (`go test` + `go vet` + `go build`) and is one commit.

**Change:** #0048 · binding #3 (network) over the `runtime.Runtime` seam.
**Spec:** `docs/superpowers/specs/2026-08-11-networked-runtime-binding-design.md` (on `docket`).

**Goal:** Expose the identical policy-free `runtime.Runtime` over a **network** transport — a WebSocket carrying binding #2's full `loop.*` JSON-RPC session (`loop.start`/`loop.send`/`loop.observe` + server-push `loop.event` tail) plus a thin stateless HTTP replay endpoint (`Attach(loop_id, from)`) — proving the seam is portable across a **third** transport with **no change to the `Runtime` interface**.

**Architecture:** Extract a **transport-agnostic dispatch core** from `internal/loopserver.Server` (today coupled to stdio via `*json.Encoder`/`*json.Decoder`). Introduce a small frame read/write `transport` abstraction that preserves the single **shared-encoder-mutex** discipline (a mid-observe id-less `loop.event` notification must never interleave with another write). Stdio becomes one transport over the core; a new WS transport is the second. Binding #2 stays behaviorally identical (all existing tests green, `internal/mcp` untouched). A new `fuse loop-serve-net` subcommand wires the WS+HTTP servers through the **same** composition root (`buildLoopServerRuntimeDeps` + `runtime.New`) that builds the multi-loop `Runtime`.

**Tech Stack:** Go 1.26.5; existing `internal/loopserver`, `internal/runtime`, `internal/event`, `cmd/fuse`; new dep `github.com/coder/websocket` (context-first WS); stdlib `net/http`.

## Global Constraints

- **Bare `go test ./...` MUST pass**, and **`go test -race ./...` MUST stay green** — the load-bearing gate; the WS live-tail + reattach path is concurrent.
- **No change to the `runtime.Runtime` interface** (`internal/runtime/runtime.go`) — binding #3 is purely a new transport (ADR-0030 policy-free-seam invariant). The runtime seam signatures are the origin/main ones: `Send(ctx, tenant event.TenantID, loopID, input)`, `Observe(ctx, tenant, loopID) (<-chan event.Event, func(), error)`, `Attach(ctx, tenant, loopID, from event.Seq) ([]event.Event, error)`.
- **No `event.Event` / `Kind` / wire-format change** (`internal/event/event.go`) — `event.Event`'s existing JSON encoding crosses the wire unchanged (D5); the `loop.event` notification frame (`eventNote`) is inherited verbatim.
- **Binding #2 stays behaviorally identical** — the extracted core drives stdio through the same frames; `internal/loopserver/server_test.go` + `reattach_test.go` pass unchanged (adapt only if a pure rename is needed, never a behavior change). **`internal/mcp` is byte-untouched.**
- **`internal/runtime` imports no `cmd/fuse`, no transport package** — the WS/HTTP binding is wired at the composition root (`cmd/fuse`) and threads the `Runtime` in as a value; the durable backend is never a construction-time shared global (`deglobalize-holder-also-per-instance-the-shared-graph`).
- **Subscribe-before-replay + dedup-at-watermark + gap-marker discipline is inherited, NOT reinvented** (`replay-live-handoff-dedup-at-watermark`). The core's `serveObserve` already implements it (`ev.Seq <= last` dedup; `ev.Seq > prev+1 && prev != 0` gap). The WS transport must not add its own replay logic.
- **The WS client read pump (test-side) MUST route id-less `loop.event` notification frames distinctly from id-keyed responses** (`mcp-read-pumps-drop-inbound-notifications`) — a demux that only matches on `id` silently drops every server-push event.
- **`tenant_id` flows through present-but-unenforced** (identity is #0049) — carry it on the wire params exactly as binding #2 does (`tenant` field), pass it to the seam, enforce nothing.
- **Any live-model verification uses a NON-Anthropic cheap gateway model via `LLM_GATEWAY_URL`** — never Claude/Anthropic/Fable/Opus/Sonnet/Haiku. The load-bearing verifications here are transport/concurrency tests (fake/scripted runtime), not live-model turns.
- **The WS reattach test MUST drive live + replay CONCURRENTLY** (force an append into the subscribe→replay gap) — a sequential test cannot see the double-delivery this path exists to prevent.

---

## File Structure

- `go.mod` / `go.sum` — MODIFY: add `github.com/coder/websocket` (D6).
- `internal/loopserver/transport.go` — CREATE: the `transport` abstraction — a `conn` the core reads request frames from and writes response + notification frames to, preserving the shared-encoder-mutex contract. Define the interface(s) the core needs: read one `req` frame, write one arbitrary frame (`encode`), and the mutex ownership.
- `internal/loopserver/server.go` — MODIFY: decouple `Server` from `*json.Encoder`/`*json.Decoder`; make `dispatch`/`handleStart`/`handleSend`/`serveObserve`/`pushEvent`/`Serve` drive the abstract `conn`/transport instead. Behavior unchanged.
- `internal/loopserver/stdio.go` — CREATE: the stdio transport (`json.Encoder`/`json.Decoder` over `io.Reader`/`io.Writer`) — the code moved out of the old `NewServer`. `NewServer(r, w, rt)` keeps its signature (constructs the stdio transport internally) so binding #2 and its tests are untouched.
- `internal/loopserver/net.go` — CREATE: the WS transport (`coder/websocket`) — reads request frames from and writes response/notification frames to one WS connection, and the thin stateless HTTP replay handler (`Attach(loop_id, from)` → durable history). A `ServeWS(ctx, conn, rt)` entrypoint that runs one session over one WS connection, and an `http.Handler` for the replay endpoint.
- `internal/loopserver/net_test.go` — CREATE: in-process `coder/websocket` client↔server tests: connect → observe(from) → live tail → drop → re-observe(from) → dedup (concurrent live+replay), asserting no loss / no dup / gap-marker-driven re-observe; plus the HTTP replay endpoint test.
- `internal/loopserver/server_test.go` / `reattach_test.go` — UNCHANGED (behavior parity guard).
- `cmd/fuse/loop_serve_net.go` — CREATE: `runLoopServeNet` — wires `buildLoopServerRuntimeDeps` + `runtime.New`, stands up the WS + HTTP `net/http` server on a `--addr`, serves until signal. Reuses the exact composition root as `runLoopServer`.
- `cmd/fuse/main.go` — MODIFY: add the `case "loop-serve-net":` dispatch + a help line.
- `cmd/fuse/loop_serve_net_test.go` — CREATE: the subcommand is registered, help lists it, and (fast) an in-process end-to-end that starts the server on `127.0.0.1:0`, drives a `loop.start`/`loop.observe` over a real WS dial against a fake/scripted Runtime, and hits the HTTP replay endpoint.

---

## Task 1 — Add the `coder/websocket` dependency (D6)

- [ ] In the feature worktree, `go get github.com/coder/websocket@latest` (pin the resolved version), then `go mod tidy`.
- [ ] Add a trivial compile-only test (or a `net.go` stub that imports it) so `go build ./...` and `go test ./...` prove the dep resolves and the untagged build still compiles. Confirm no other module churn.
- [ ] Verify `go test ./...` and `go build ./...` green. **Commit:** `build(deps): #48 add github.com/coder/websocket for binding #3`.

## Task 2 — Extract the transport-agnostic dispatch core (binding #2 unchanged)

**TDD:** the existing `internal/loopserver/server_test.go` + `reattach_test.go` ARE the regression gate — they must pass byte-for-byte-behaviorally after the refactor. Add no new behavior.

- [ ] Define the `transport`/`conn` abstraction in `transport.go`: a frame **reader** (`readRequest(ctx) (req, error)` returning `io.EOF` to end), a frame **writer** (`encode(v any) error`) that owns the shared mutex serialization, matching today's `encode()` contract. Keep `req`/`resp`/`eventNote` frame types where they are (or move to `transport.go`) — no field changes.
- [ ] Refactor `Server` to hold a `conn` (or transport) instead of `enc`/`dec`. Move `Serve`'s decode loop to `conn.readRequest`; route `encode` through `conn.encode`. `serveObserve`'s pump goroutine still calls `pushEvent → encode`, so the mutex still lives with the transport (one mutex per connection).
- [ ] Move the stdio-specific construction into `stdio.go`: a `newStdioTransport(r io.Reader, w io.Writer) *stdioTransport` implementing the abstraction over `json.Encoder`/`json.Decoder` with the `encMu`. `NewServer(r, w, rt)` builds the stdio transport internally — **signature unchanged**.
- [ ] Preserve the fail-fast decode-error policy (one null-id `codeParseError` frame, then return) — it belongs to the stdio transport's `readRequest` (a streaming `json.Decoder` cannot resync); the WS transport reads discrete messages so it has its own per-message parse-error handling.
- [ ] Run `go test ./internal/loopserver/... -race` — existing tests green. Run `git diff --stat internal/mcp` → empty (byte-untouched guard). **Commit:** `refactor(loopserver): #48 extract transport-agnostic dispatch core; stdio is one transport`.

## Task 3 — WS transport: full `loop.*` session over one connection (D3)

**TDD:** write `net_test.go` first with an in-process `coder/websocket` client↔server harness over a **fake Runtime** (a scripted `runtime.Runtime` test double whose `Observe`/`Attach` feed a controllable event channel + history slice, mirroring the doubles in `server_test.go`).

- [ ] Test A (happy path): dial WS, send `loop.start` → get `loop_id`; `loop.observe` from 0 → get `observeResult` then replayed `loop.event` frames then live-tail frames; assert each event's `seq` seen exactly once and in order. The client read pump routes **id-keyed** frames to response waiters and **id-less** `loop.event` frames to an event channel (guard against the `mcp-read-pumps-drop-inbound-notifications` trap).
- [ ] Test B (concurrent reattach dedup — LOAD BEARING): drive an append **into the subscribe→replay gap** concurrently (a `runtime` double that lets the test inject an event after `Observe` returns but before `Attach`'s history is read), disconnect, re-observe `from=<lastSeq>`; assert no duplicate `seq`, no lost `seq`, and that a forced hole yields exactly one `gap:true` frame driving re-observe. This exercises the inherited `serveObserve` over the real WS transport (`replay-live-handoff-dedup-at-watermark`).
- [ ] Implement `net.go`: `newWSTransport(c *websocket.Conn) *wsTransport` implementing the `conn` abstraction — `readRequest` = `c.Read(ctx)` → unmarshal one JSON-RPC frame; `encode` = marshal + `c.Write(ctx, MessageText, ...)` under a per-connection mutex (WS `Write` is not concurrency-safe). `ServeWS(ctx, c, rt)` builds a `Server` over the WS transport and calls `Serve(ctx)`.
- [ ] Handle WS close cleanly: on client drop / `ctx` cancel, `Serve` returns, the observe pump goroutine's `cancel()` fires (it already `defer cancel()`s), the subscription is released — assert no goroutine leak (a `goleak`-style check or channel-close assertion).
- [ ] `go test ./internal/loopserver/... -race` green. **Commit:** `feat(loopserver): #48 WebSocket transport — full loop.* session over one connection`.

## Task 4 — Thin stateless HTTP replay endpoint (D3)

**TDD:** test first with `httptest.NewServer` against the fake Runtime.

- [ ] Test: `GET /loops/{id}/events?from=<seq>` (tenant carried per the resolved shape — a `?tenant=` query param or `X-Tenant` header, unenforced) returns the durable history JSON array (`[]event.Event` with `seq > from`), Content-Type `application/json`. Assert the response equals `rt.Attach(ctx, tenant, id, from)`. Assert no live tail, no connection state (a second identical request returns the same bytes — idempotent/stateless).
- [ ] Test the error mapping: unknown loop → 404 (or a JSON error body), malformed `from` → 400. Keep it stateless — no `loop.start`/`loop.send` on HTTP (D3; those are WS-only).
- [ ] Implement the `http.Handler` in `net.go`: parse `{id}`/`from`/tenant, call `rt.Attach`, marshal the slice. Use Go 1.22+ `http.ServeMux` path patterns (`GET /loops/{id}/events`).
- [ ] `go test ./internal/loopserver/... -race` green. **Commit:** `feat(loopserver): #48 stateless HTTP replay endpoint (Attach catch-up)`.

## Task 5 — `fuse loop-serve-net` subcommand through the shared composition root

**TDD:** test first — register + help + an in-process end-to-end.

- [ ] Test A: `loop-serve-net` is a registered `switch args[0]` case and `fuse help` lists it (mirror `TestLoopServerDispatchRegistered` / `TestHelpListsLoopServer`).
- [ ] Test B (fast E2E): start `runLoopServeNet` bound to `127.0.0.1:0` with a fake/scripted Runtime (swap the runtime builder behind a test seam the way `stdinForLoopServer` is swapped in `loop_server.go`), read the chosen port, dial the WS, drive `loop.start` + `loop.observe`, and hit the HTTP replay endpoint — assert the frames. Shut down cleanly on context cancel.
- [ ] Implement `cmd/fuse/loop_serve_net.go`: `runLoopServeNet(args, cfg, reg, stdout, stderr) int` — parse `--addr` (default e.g. `127.0.0.1:8787`), load skills/tools exactly as `runLoopServer`, call `buildLoopServerRuntimeDeps(...)` + `runtime.New(deps)` (auto-approve policy, real fsstore, durable backend — reuse verbatim), mount the WS handler (`websocket.Accept` → `ServeWS`) and the HTTP replay handler on one `http.ServeMux`, `http.Serve` until signal/ctx. Document the auto-approve binding choice in the doc comment (ADR-0028 stance).
- [ ] Add the `case "loop-serve-net":` dispatch + help line to `main.go`.
- [ ] `go test ./cmd/fuse/... -race` and `go test ./... -race` green. **Commit:** `feat(cmd/fuse): #48 loop-serve-net subcommand — WS+HTTP binding #3 over the multi-loop Runtime`.

## Task 6 — Whole-suite green + parity guard + tidy

- [ ] `go build ./...`, `go vet ./...`, `go test ./... -race` — all green.
- [ ] Confirm `git diff origin/main -- internal/mcp` is empty (binding-#2-sibling byte-untouched invariant) and binding #2's tests are unchanged in behavior.
- [ ] `go mod tidy` clean; no stray deps. Confirm the untagged build never imports Postgres/pgx (unchanged from #47).
- [ ] Final commit if any tidy-up: `chore(loopserver): #48 tidy + whole-suite green`.

---

## Verification (per project policy — cheap gateway, never Claude)

The load-bearing behaviors here (transport framing, concurrent reattach dedup, gap markers, HTTP replay) are proven by the `-race` Go tests above against a scripted/fake Runtime — no live model needed. If an end-to-end sanity run against a real loop is wanted, drive `fuse loop-serve-net` with `LLM_GATEWAY_URL` pointed at a **cheap non-Anthropic gateway model** (the `verify-tool-loop-at-gateway-seam` pattern), attach a WS client, and observe a real `loop.event` tail. Never Claude/Anthropic/Fable/Opus/Sonnet/Haiku.

## Notes / deviations to watch

- **Open q1 (structuring)** resolved to: extracted core **inside** `internal/loopserver` with `transport.go` + `stdio.go` + `net.go` — least churn, `NewServer` signature preserved. If the WS transport forces the core into a sibling package to avoid an import cycle, that is an acceptable D-deviation — record it.
- **Open q2 (HTTP shape)** resolved to: `GET /loops/{id}/events?from=` stateless replay; tenant carried unenforced. If a JSON-RPC `Attach` POST proves cleaner for the SDK later, that is #0050's call, not this change's.
- **ADR (Step 6):** record the binding-#3 transport decision (WS-full-session + thin-HTTP-replay, shared core, `coder/websocket`) relating to ADR-0028 and the multi-loop/durable-store ADRs.
