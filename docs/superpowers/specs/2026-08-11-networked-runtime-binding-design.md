<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0048 — Networked binding over the Runtime seam — WS live observe + HTTP start/send/replay](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-11-0048-networked-runtime-binding.md)**
<!-- docket:backlink:end -->

# Networked binding over the Runtime seam — design

> Change #0048 · binding #3 (network) over the `runtime.Runtime` seam.
> Depends on #0046 (multi-loop host — **done**) and #0047 (durable/distributed event store — **implemented**, PR #50).

## Problem

The `runtime.Runtime` seam (change #0045) is a policy-free interface — `StartLoop` / `Send` /
`Spawn` / `Observe` / `Attach`, keyed by `loop_id` (`tree.RootID()`) — with two bindings today:
the CLI (binding #1) and the headless **stdio** `fuse loop-server` JSON-RPC server (binding #2,
`internal/loopserver`). Change #0046 made the event store per-loop instance state so one process
hosts N concurrent loops; change #0047 makes a loop's existence + history durable and reachable
from any instance.

What is still missing is the milestone that makes hosting *remote*: exposing the identical seam
over a **network** transport so a client on another machine — "attach to your running loop from
your phone after the server redeployed" — can drive a loop. Binding #2 speaks JSON-RPC only over
`os.Stdin`/`os.Stdout` (its `Server` is hard-wired to `*json.Encoder`/`*json.Decoder` over
`io.Reader`/`io.Writer`), so it is unreachable over the network. This change adds binding #3 and,
in doing so, proves the seam is portable across a *third* transport with **no change to the
`Runtime` interface itself** — the load-bearing "same seam, three bindings" proof.

## Decisions (resolved at grooming)

### D1 — Reuse binding #2's protocol; new transport, shared core

Binding #3 keeps the **exact `loop.*` JSON-RPC 2.0 method set and event framing** of binding #2
(`loop.start` / `loop.send` / `loop.observe`, id-less `loop.event` push notifications with gap
markers, `-32700`/`-32601` error frames). Only the transport changes: stdio → network I/O. To
avoid protocol drift between the two bindings, extract a **transport-agnostic dispatch core** out
of `internal/loopserver.Server` so both stdio and network drive one implementation of `dispatch` /
`handleStart` / `handleSend` / `serveObserve` / `pushEvent`.

Concretely, `internal/loopserver.Server` today couples dispatch to concrete stdio via
`enc *json.Encoder` / `dec *json.Decoder`. The refactor introduces a small frame-read / frame-write
abstraction (a "conn" the core reads request frames from and writes response + notification frames
to, preserving the single **encoder-mutex** discipline that keeps a mid-observe `loop.event`
notification from interleaving with another write). Stdio implements it over
`json.Encoder`/`json.Decoder`; the network transport implements it over the WS connection (and, for
HTTP, a one-shot request/response frame pair). **`serveObserve`'s subscribe-before-replay +
dedup-at-watermark + gap-marker discipline is inherited verbatim** — it is already proven and is
exactly what remote reattach needs; binding #3 must not reinvent it.

This is the same "reuse the proven framing, own the dispatch" spirit ADR-0028 applied when binding
#2 chose a new `internal/loopserver` over extending `internal/mcp`. Whether the extracted core
lives in `internal/loopserver` (with `stdio.go` + `net.go` transports) or a new sibling package is
a build-time structuring call; the invariant is **one dispatch, N transports**, and binding #2 stays
behaviorally identical (its existing tests, including the `git diff --exit-code` byte-untouched
guard on `internal/mcp`, still pass).

### D2 — Build on #0047's durable store + registry (`depends_on: 47`)

Binding #3 is designed directly on #0047's **durable EventStore + durable loop registry** seam:
cross-instance reattach (a client re-observes a loop a *different* instance started), cold-process
`(tenant_id, loop_id)` resolution, and the shared pub/sub live tail (Postgres `LISTEN`/`NOTIFY`)
are what make "attach from your phone after the server redeployed" actually work over the wire.
`47` is added to `depends_on` so the implementer's reconcile pass will not build `48` until `47`
is `done` (PR #50 merged). `tenant_id` is **present but unenforced** here (it flows through the seam
per #0047's D2); *who a tenant is and how identity is proven* is change #0049, explicitly out of
scope.

If `47` slips, `48` waits — that is the correct hosting-arc ordering (storage-durable before
network-exposed), not an accident.

### D3 — WS carries the full session; HTTP is a thin replay endpoint

One **WebSocket** connection carries the entire `loop.*` JSON-RPC protocol —
`loop.start` / `loop.send` / `loop.observe` request frames and the server-push `loop.event`
notification stream — i.e. binding #2's exact stdio protocol, now over WS. This keeps the network
binding as close to binding #2 as possible: the WS connection *is* the bidirectional byte stream
the stdio core already assumes.

**HTTP** exists as a thin, **stateless** replay endpoint for late joiners / clients that only want
catch-up without a live tail — a single `Attach(loop_id, from)` → durable history request/response
(`GET`-shaped, cacheable-friendly, no connection state). This satisfies the stub's explicit
"HTTP for replay" half without splitting the live protocol across two transports. The HTTP surface
carries no `loop.start`/`loop.send` in this change (the full-session WS path owns those); adding
REST-native start/send later is a client-SDK ergonomics decision (change #0050) and is out of scope.

> Note (recorded): the stub's literal wording was "WS for live observe, HTTP for start/send/replay."
> Grooming deliberately chose **WS-full-session** over method-splitting because splitting
> `loop.send` (which must interleave with the same loop's live `loop.event` tail) across two
> transports reintroduces exactly the cross-transport ordering problem the single stdio stream
> avoids. HTTP stays for the stateless catch-up case, where request/response is the natural shape.

### D4 — Reconnect: client tracks last Seq, re-observe from cursor (stateless server)

The server holds **no per-connection resume state**. On a WS drop the client remembers the highest
`event.Seq` it received and reconnects by re-opening `loop.observe` with `from=<lastSeq>`; the
server runs the inherited subscribe-before-replay + dedup-at-watermark path (D1), so the client
misses nothing and sees no duplicate. A **gap marker** (a `loop.event` whose `Seq` skips) tells the
client to re-observe from its last contiguous seq. This reuses binding #2's exact discipline over
the wire and keeps the server stateless — no resume tokens, no session expiry, no per-connection
bookkeeping — which #0047's cheap durable `Replay(from)` makes practical.

### D5 — Wire format: reuse `event.Event` JSON; `loop_id` as the handle

Events cross the wire in `event.Event`'s **existing JSON encoding** — the same shape `fsstore`
writes and binding #2 already emits as `loop.event` params. The addressable loop handle is the
`loop_id` string (`tree.RootID()`), exactly as binding #2 keys it. **No new network envelope, no
new handle DTO** — binding #3 inherits the wire format for free and stays byte-compatible with
binding #2's protocol, so the same client codec works against either transport. (A versioned
external-SDK envelope, if ever wanted, is change #0050's concern, not this binding's.)

### D6 — WebSocket library: `github.com/coder/websocket`

No WS library is in `go.mod` today, so this is a real dependency decision. Use
**`github.com/coder/websocket`** (the maintained successor to `nhooyr.io/websocket`): a minimal,
**context-first** API (`Read`/`Write` take `context.Context`) that maps cleanly onto the seam's
`ctx` threading and #0047's ctx-first store ops, with ~zero transitive deps and a stdlib-idiomatic
surface. Rejected: `gorilla/websocket` (callback/deadline API, needs manual ctx bridging).

## What changes

1. **Transport-agnostic dispatch core** extracted from `internal/loopserver.Server` — one
   implementation of `dispatch`/`handleStart`/`handleSend`/`serveObserve`/`pushEvent` over an
   abstract frame read/write conn, preserving the encoder-mutex discipline. Stdio becomes one
   transport over the core; binding #2 stays behaviorally identical (existing tests + the
   `internal/mcp` byte-untouched guard still green).
2. **Network transport (binding #3)** — a `coder/websocket` server that runs the full `loop.*`
   JSON-RPC session over one WS connection, plus a thin stateless HTTP `Attach(loop_id, from)`
   replay endpoint. Both drive the shared core; both stay headless / policy-free (no
   renderer/TUI/approval gate — auto-approve is the binding's documented choice, per ADR-0028's
   stance).
3. **A `fuse` subcommand** to serve the network binding (mirroring how `loop-server` is a
   `switch args[0]` case in `cmd/fuse/main.go`), wired through the *same* composition root that
   builds the multi-loop `Runtime` (`buildLoopServerRuntimeDeps` in `cmd/fuse`), so the network
   binding hosts N concurrent loops over #0046's per-loop state and, once #0047 lands, over the
   durable store + registry.
4. **`event.Event` / `loop_id`** cross the wire unchanged (D5); `tenant_id` flows through the seam
   present-but-unenforced (D2).
5. **New dependency:** `github.com/coder/websocket` in `go.mod`.

## Out of scope

- **Auth / multi-tenancy / tenant identity** — change #0049. This binding carries `tenant_id`
  through the seam but proves no identity and enforces no boundary of its own.
- **Any change to the `Runtime` interface** — binding #3 is purely a new transport over the
  existing seam; if the seam needs to change, that is a different change.
- **REST-native `loop.start`/`loop.send` over HTTP** and a **versioned external-SDK wire envelope**
  — client-SDK ergonomics, change #0050.
- **Observability emission** (OTEL spans, `/metrics`) — change #0051; #0047 already threaded the
  ctx hooks this binding inherits.
- **TLS / deployment topology / load-balancing** across instances — operational concerns layered on
  top; the binding is transport, not deployment. (#0047's shared pub/sub is what *enables*
  multi-instance; wiring a specific deployment is not this change.)

## Open questions (resolve at build reconcile)

- **Where the extracted core lives** — keep it inside `internal/loopserver` (add `transport`
  abstraction + `stdio.go`/`net.go`) vs. a new sibling package. Structuring call; the invariant is
  one dispatch / N transports and binding #2 unchanged.
- **HTTP replay endpoint shape** — exact path/verb (`GET /loops/{id}/events?from=` vs a JSON-RPC
  `Attach` POST) and how `tenant_id`/`loop_id` are carried in the request, given identity is
  unenforced here (#0049). Keep it stateless either way.
- **Testing the WS path without flakiness** — an in-process `coder/websocket` client↔server test
  exercising connect → observe(from) → live tail → drop → re-observe(from) → dedup, asserting no
  loss / no dup / gap-marker-driven re-observe. Confirms D1+D4 over the real transport, not a fake.
  (Ref the `replay-live-handoff-dedup-at-watermark` learning — a sequential test cannot see the
  double-delivery this path exists to prevent; the test must drive live + replay concurrently.)
- **Reconcile against #0047 at build time** — whether #0047 has merged, and the exact durable
  EventStore/registry symbols the network binding resolves loops through (the in-memory `r.loops`
  cache-over-durable-registry shape #0047 establishes).
- **ADR at Step 6** — record the binding-#3 transport decision (WS-full-session + thin-HTTP-replay,
  shared core, `coder/websocket`) as a new ADR relating to ADR-0028 (binding #2's JSON-RPC-not-MCP
  decision) and ADR-0030/0031 (multi-loop + durable store this sits on).
