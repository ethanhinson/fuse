<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0055 — Connect/protobuf transport — IDL-defined loop.* wire, successor to](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0055-grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4.md)**
<!-- docket:backlink:end -->

# gRPC/Connect transport — a protobuf-IDL `loop.*` wire, successor to #48

## Context

Change **#48** (done, ADR-0032) shipped the networked binding on **JSON-over-WebSocket**
(`coder/websocket`) + **JSON-over-HTTP**, reusing binding #2's `loop.*` JSON-RPC protocol and
`event.Event`'s existing JSON encoding. That was the right call to prove a networked binding fast,
but it leaves the **cross-language contract implicit**: a TS/Python client and the Go server share no
schema, so every non-Go SDK (starting with #50's TS SDK for the Wander testbed) must hand-track the
envelope. This change makes the wire **schema-first** and reopens ADR-0032 deliberately.

**Motivating consumer — a testbed, not a product.** The **Wander** app (change #50's consumer) is a
throwaway browser prototype whose only job is to **exercise this stack end-to-end** — start/drive/
observe/replay a hosted loop from a browser. So #55 optimizes for *proving the SDK path works,
browser included, with one schema*, not for production deployment hardening. That framing drives the
decisions below toward the simplest thing that genuinely proves the path.

The `loop.*` protocol #55 re-homes onto a typed contract (from #48 / binding #2):

- `loop.start(task, model[, tenant]) → loop_id` — **unary**
- `loop.send(loop_id, input[, tenant])` — **unary**
- `loop.observe(loop_id, from_seq[, tenant]) → stream of loop.event` — **server-streaming**
- `Attach(loop_id, from)` durable replay — subsumed by `loop.observe(from_seq=…)` (a stream that
  replays history then continues live), or kept as a unary catch-up call (plan-time).

The reattach discipline that must be preserved verbatim (inherited from #48 / binding #2): **subscribe
before replay**, **dedup at the replay watermark** (`Seq <= last`), **gap markers** driving re-observe
(ADR-0025 drop-newest), `loop_id = tree.RootID()` as the handle, `event.Seq` as the cursor.

**Decided in grooming (2026-08-11):**

1. **Transport = Connect (connectrpc.com), protobuf IDL, superseding ADR-0032.** Connect over classic
   gRPC because it is **browser-native without a proxy**: `connect-es` (TS) speaks unary and
   **server-streaming** directly to a `connect-go` server over HTTP, so Wander reaches the loop wire
   with no Envoy/grpc-gateway in the way. Same protobuf schema generates the Go server, the Go SDK
   backend, and the TS client.
2. **Only unary + server-streaming are used — no bidi.** The `loop.*` protocol maps cleanly:
   `loop.start` / `loop.send` are discrete **unary** calls; `loop.observe` is a **server-stream** of
   events. The client sends input via separate unary `Send` calls, never by writing into the observe
   stream — so no bidirectional stream is needed. This matters: **Connect bidi streaming does NOT work
   in browsers** (it needs full-duplex HTTP/2 that browser fetch can't express), but our protocol
   never needs it. Unary + server-streaming both work in-browser via `connect-es`.
3. **Replace #48's JSON wire outright — no dual-wire transition.** #55 supersedes ADR-0032's transport;
   the JSON WS/HTTP binding is removed and the Connect/protobuf wire is the only networked binding.
   Safe precisely because #48's wire has **no consumers yet** — the SDKs (#50) are unbuilt and nothing
   else drives it — so a transition period would add dual-maintenance for no benefit.
4. **protobuf is the IDL** (mandatory with Connect; also the mature multi-language choice). Cap'n Proto
   was considered and dropped — its Go RPC + browser tooling is far less mature, and a testbed's point
   is to exercise the SDK path, not the serializer.
5. **Generated stubs are the contract #50 consumes.** #55 emits Go (`connect-go`) and TS
   (`connect-es`) stubs + a versioned proto package; #50's Go remote backend and TS SDK generate their
   clients from it. #55 owns the wire; #50 owns the ergonomics over it.

## Decision

### A Connect/protobuf transport replacing the JSON binding

Define the `loop.*` service and the `event.Event` message in a **protobuf schema** (package versioned
from day one, e.g. `fuse.loop.v1`), and serve it with **`connect-go`**, reusing #48's extracted
**transport-agnostic dispatch core** — the core stays; only the frame read/write layer changes from
JSON-WS/HTTP to Connect. Concretely:

- `rpc StartLoop(StartLoopRequest) returns (StartLoopResponse)` — unary; wraps `Runtime.StartLoop`.
- `rpc Send(SendRequest) returns (SendResponse)` — unary; wraps `Runtime.Send`.
- `rpc Observe(ObserveRequest) returns (stream Event)` — server-streaming; wraps
  `Runtime.Observe` + the durable `Runtime.Attach(from)` replay, emitting history-then-live with the
  inherited subscribe-before-replay + dedup-at-watermark + gap-marker discipline.
- `event.Event` is expressed as a protobuf message (`Seq`, tenant, the event payload) — the wire
  encoding moves from #48's hand-rolled JSON to the generated protobuf, so Go and TS agree by
  construction. Whether Go keeps `internal/event.Event` as the domain type and maps to/from the proto
  at the transport edge, or adopts the generated type deeper, is a plan-time call (favor a thin
  edge-mapping to keep `internal/event` transport-free).

The `internal/loopserver` stdio binding (binding #2) and the `Runtime` seam are **untouched** — #55
changes only the *networked* transport. The extracted dispatch core is the reuse point.

### Browser reach via connect-es, no proxy (decisions 1–2)

The TS client (`connect-es`, consumed by #50's TS SDK) calls unary `StartLoop`/`Send` and opens the
`Observe` **server-stream** directly against the `connect-go` server. Reconnect is client-driven: the
browser tracks the last `event.Seq` and re-opens `Observe(from_seq=<lastSeq>)`; the inherited
subscribe-before-replay + dedup-at-watermark path guarantees no loss / no dup. **Every** post-open
stream error is treated as a clean shutdown-and-reconnect, not a server fault — the Connect-error-model
analogue of learning `websocket-read-errors-are-not-closeerror` (which warned that abnormal peer close
never arrives as the "clean" error type). No grpc-web, no Envoy, no proxy tier.

### Streaming model & the acceptance test (testbed framing)

The one thing #55 must *prove* is a **long-lived `Observe` server-stream surviving reconnect with
correct replay/dedup**, from both a Go client and a browser (`connect-es`) client. The test must drive
**live + replay concurrently** — force an append into the subscribe→replay gap — or it cannot see the
double-delivery the watermark dedup exists to prevent (learning
`replay-live-handoff-dedup-at-watermark`; a sequential test is blind to it). Live verification uses a
cheap scripted `LLM_GATEWAY_URL` double, never Claude/Anthropic (project policy).

### tenant pass-through, unchanged

`tenant_id` continues to flow over the wire **present-but-unenforced** (identity is #49) — now a
typed field on the proto requests rather than an untyped JSON field. #55 changes the encoding, not the
trust model.

## Consequences

**Enables.** One protobuf schema is the single source of truth for the `loop.*` wire, and every SDK
(#50's Go + TS, later Python/mobile) generates its client from it — no hand-tracked envelope, no
drift. The browser reaches the wire natively (`connect-es`, no proxy), so the Wander testbed can
exercise start/drive/observe/replay end-to-end. #48's "extract a transport-agnostic dispatch core"
investment pays off exactly as intended: a third transport swaps in under the same core.

**Costs / gives up.** A protobuf toolchain enters the repo (buf or protoc, `connect-go` +
`connect-es` codegen, generate-and-check-in vs. build-step wiring — plan-time). #48's JSON WS/HTTP
binding is **removed** (ADR-0032 superseded), so the `coder/websocket` binding and its tests are
retired — a real deletion, justified by zero consumers. Reconnect/replay must be **re-proven** over
Connect streaming (the WS learnings carry over in spirit but not in code). Bidi is off the table in
the browser — accepted, because the protocol doesn't use it.

**Dependencies.** `depends_on: []` — #55 is **independently buildable** now; it supersedes #48 but
does not depend on it (#48 is done, and #55 replaces its transport). #55 **gates #50**: both SDKs
generate from #55's proto, so #50 (and the Wander testbed) cannot build until #55 lands. #55 does
**not** depend on #49 — tenant stays pass-through/unenforced here, exactly as #48 shipped it.

## Out of scope

- **The SDKs themselves** (Go + TS ergonomics, local backend, credential seam) — change #50, which
  consumes #55's generated stubs.
- **Auth / identity enforcement** — change #49; #55 keeps tenant pass-through/unenforced.
- **A Python / mobile SDK** — later; #55's proto is what makes them cheap.
- **Any change to the `Runtime` seam or the stdio binding #2** — #55 changes only the networked
  transport, reusing the extracted dispatch core.
- **Production deployment hardening** (TLS termination topology, load-balancing, multi-instance
  routing, auth infra) — Wander is a testbed; operational concerns are out of scope here.
- **Observability emission** (OTEL / `/metrics`) — change #51.

## Open questions (for plan-time reconcile)

- protobuf toolchain: **buf** vs. raw `protoc`; **generate-and-commit** the Go/TS stubs vs. a build
  step; where the generated packages live (Go module path; TS package layout shared with #50).
- Whether `Attach` stays a distinct unary replay RPC or fully collapses into `Observe(from_seq)` as a
  history-then-live server-stream (the latter is simpler and is the recommended default).
- Whether Go adopts the generated protobuf `Event` deeper or maps at the transport edge to keep
  `internal/event` transport-free (favor edge-mapping).
- Connect streaming framing over HTTP/1.1 vs. HTTP/2 for the `Observe` stream, and confirming
  `connect-es` server-streaming behaves under a real browser reconnect (the core testbed assertion).
- Exact retirement of #48's `coder/websocket` binding + tests (delete vs. keep behind a build tag for
  reference) — default: delete, since ADR-0032 is superseded and nothing consumes it.
