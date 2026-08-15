---
id: 55
slug: grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4
title: "Connect/protobuf transport — IDL-defined loop.* wire, successor to #48"
status: done
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: []
related: [46, 47, 48, 50]
discovered_from: [50]
adrs: [32, 33]
spec: docs/superpowers/specs/2026-08-11-grpc-connect-transport-design.md
plan: docs/superpowers/plans/2026-08-11-grpc-connect-transport-plan.md
results: docs/results/2026-08-11-grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4-results.md
trivial: false
auto_groomable:
branch: feat/grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/52
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-grpc-connect-transport-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-grpc-connect-transport-design.md) |
| Plan | [2026-08-11-grpc-connect-transport-plan.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/2026-08-11-grpc-connect-transport-plan.md) |
| Results | [2026-08-11-grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-08-11-grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4-results.md) |
| PR | [#52](https://github.com/ethanhinson/fuse/pull/52) |
| ADRs | [ADR-0032](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0032-binding-3-websocket-session-http-replay-shared-dispatch.md), [ADR-0033](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0033-networked-binding-connect-protobuf-fuse-loop-v1.md) |
<!-- docket:artifacts:end -->

## Why

Change 48 (done, ADR-0032) pinned the networked binding's wire as JSON-over-WebSocket
(`coder/websocket`) + JSON-over-HTTP, reusing binding #2's `loop.*` JSON-RPC protocol and
`event.Event`'s existing JSON encoding. That was the right call to prove the transport quickly, but
it leaves the cross-language contract implicit: a TS/Python client and the Go server share no schema,
so every non-Go SDK (starting with the TS SDK in change 50) must hand-track the envelope or generate
types from the Go structs.

A schema-first IDL fixes that at the root: define the `loop.*` protocol and `event.Event` shape once
in **protobuf** and carry it over a **Connect (connectrpc)** transport, so every binding (Go, TS,
future Python/mobile) generates its client from one authoritative contract. This is the classic answer
to "many-language clients over one wire" — deferred at ADR-0032 only because #48 needed to ship. This
change reopens that decision deliberately, as a **successor to #48 that supersedes ADR-0032**.

**Wander (change 50's consumer) is a throwaway testbed**, so #55 optimizes for *proving the SDK path
end-to-end, browser included, with one schema* — not production hardening. See the linked spec for the
full design; at proposal altitude:

## What changes

- **Connect + protobuf, replacing #48's JSON wire outright.** Define `loop.*` + `event.Event` in a
  versioned protobuf schema and serve it with `connect-go`, **reusing #48's extracted
  transport-agnostic dispatch core** (only the frame layer changes). The `internal/loopserver` stdio
  binding (#2) and the `Runtime` seam are untouched. #48's `coder/websocket` binding is **removed**
  (ADR-0032 superseded) — safe because nothing consumes it yet (the SDKs are unbuilt).
- **Connect for browser reach with NO proxy.** Chosen over classic gRPC because `connect-es` (TS)
  speaks **unary + server-streaming** directly to a `connect-go` server over HTTP — so Wander reaches
  the wire with no Envoy/grpc-gateway. The `loop.*` protocol maps cleanly: `loop.start`/`loop.send` are
  **unary**, `loop.observe` is a **server-stream** of events. **No bidi** is used (and bidi wouldn't
  work in browsers anyway — the protocol never needs it).
- **Reattach discipline preserved.** `loop.observe(from_seq)` is a history-then-live server-stream that
  keeps subscribe-before-replay + dedup-at-watermark + gap markers; client tracks `event.Seq` and
  re-opens the stream on reconnect. Every post-open stream error is a clean reconnect, not a fault.
- **Generated stubs are the contract change 50 consumes** — Go (`connect-go`) + TS (`connect-es`) from
  one proto. #55 owns the wire; #50 owns the SDK ergonomics over it.
- **Resilient to ingress conditions (in scope).** The loops are an application behind a reverse
  proxy / gateway; #55's transport is written to that reality: a dropped `Observe` stream is normal and
  a reconnect may land on a **different instance** and must still replay (an architectural reliance on
  #46 multi-loop hosting + #47 durable cross-instance store — no sticky sessions assumed); **parked**
  loops' idle streams survive via keepalive or cleanly re-establish; forwarded-header/scheme awareness
  when TLS terminates at ingress; deadline/retry safety through an intermediary. #55's tests **prove**
  the different-instance reconnect and the idle-parked cases.
- **tenant stays pass-through/unenforced** (a typed proto field now); identity is change 49.

## Out of scope

- The **SDKs** (Go + TS, local backend, credential seam) — change 50, consuming #55's stubs.
- **Auth / identity enforcement** — change 49; #55 keeps tenant pass-through.
- **Ingress *configuration*** — provisioning TLS certs, choosing/configuring the specific proxy /
  gateway / load balancer, deployment topology. Environment concerns — *distinct from* being resilient
  to the conditions they create, which is in scope (above). #55 owns the resilience; the environment
  owns the config.
- **Any change to the `Runtime` seam or stdio binding #2** — #55 changes only the networked transport.
- **Observability** (OTEL / `/metrics`) — change 51.

## Open questions

- protobuf toolchain (buf vs. protoc; generate-and-commit vs. build step) and where the generated
  Go/TS packages live (shared with change 50's TS package).
- Whether `Attach` collapses fully into `Observe(from_seq)` (recommended) or stays a distinct unary
  replay RPC.
- Whether Go maps protobuf `Event` at the transport edge to keep `internal/event` transport-free
  (favored) or adopts the generated type deeper.
- Confirming `connect-es` server-streaming under a real browser reconnect — the core testbed assertion.

## Reconcile log

### 2026-08-11 — reconcile against origin/main (build-time)

Spec verified accurate against `origin/main` (the base the feature branch cuts from). No fundamental
invalidation; scope holds. Findings folded in:

- **#48's binding IS on `origin/main` exactly as the spec/ADR-0032 describe.** `internal/loopserver/`
  carries the extracted dispatch core (`transport.go`: the `conn` interface — `readRequest(ctx)
  (req, error)` returning `io.EOF` to end, `encode(v any) error` owning the one-mutex-per-conn write
  serialization — plus the shared `req`/`resp`/`startParams`/`sendParams`/`observeParams`/`eventNote`
  frame types), the stdio transport (`stdio.go`), and the network binding (`net.go`: `ServeWS(ctx,
  *websocket.Conn, rt)` over `github.com/coder/websocket` + `NewReplayHandler(rt)` exposing
  `GET /loops/{id}/events?from=&tenant=` → `rt.Attach`). `go.mod` on origin/main has the
  `coder/websocket` dependency. So the reuse point (dispatch core + `serveObserve`'s
  subscribe-before-replay + dedup-at-watermark `ev.Seq <= last` + gap-marker `ev.Seq > prev+1 &&
  prev != 0`) and the deletion target (WS binding + HTTP replay + `coder/websocket` dep) both exist —
  #55 proceeds as written. (NOTE: the local checkout was ~20 commits behind origin/main and did not
  have these files; irrelevant because the feature branch cuts from `origin/main`, but recorded as a
  build-loop lesson to harvest — always inspect the integration branch, not the local tree.)
- **The `runtime.Runtime` seam is untouched and already satisfies the multi-instance requirement.**
  `internal/runtime/runtime.go` — `StartLoop(ctx, LoopConfig) (LoopHandle, error)`, `Send(ctx, tenant,
  loopID, input) error`, `Observe(ctx, tenant, loopID) (<-chan event.Event, func(), error)`,
  `Attach(ctx, tenant, loopID, from) ([]event.Event, error)`. `Observe`/`Attach` already resolve a loop
  via the durable registry (cross-instance), so spec Resilience §1 (reconnect-to-a-different-instance
  replays over #47's durable store) is satisfiable with NO seam change — the Connect `Observe` RPC wraps
  exactly this. #46 (multi-loop host) and #47 (durable store: `internal/event/{store,registry}.go`,
  `pgstore`, `fsstore`) are `done` on origin/main.
- **`event.Event` carries NO tenant field** (`Seq, TS, NodeID, ParentID, Depth, Turn, Kind, Payload`);
  tenant flows through the seam METHODS, not the event struct. So the proto `Event` message mirrors
  those 8 fields; `tenant_id` is a typed field on the *request* messages (StartLoop/Send/Observe),
  pass-through/unenforced — not on the event. Adjusts the spec's "Seq, tenant, the event payload"
  wording to: request-level tenant, event-level Seq+payload.
- **protobuf/Connect toolchain is NOT yet present** — no `connectrpc`/`bufbuild`/`google.golang.org/
  protobuf` in `go.mod`/`go.sum`; no `buf`/`protoc`/`protoc-gen-go` on PATH; no `*.proto`/`buf.yaml`.
  `node`/`npm` ARE available (for `connect-es`). Plan-time default (open question 1): **buf +
  generate-and-commit** the Go (`connect-go`) and TS (`connect-es`) stubs, so the build has no live
  codegen dependency and CI stays hermetic; pin plugin versions in `buf.gen.yaml`.
- **Generated TS stub location (informed at build-time):** change 50's TS SDK will live as an npm
  workspace in THIS repo (`sdk/ts`), Wander a sibling workspace. So the connect-es output lands inside
  the repo's TS workspace layout (e.g. `proto/gen/ts` or `sdk/ts/gen`) — importable by #50's SDK as a
  sibling — rather than a separate repo. Go stubs use the normal Go module path
  (`github.com/ethanhinson/fuse/...`). Codegen-output location only; no scope/acceptance-test change.
- **Gateway-double pattern is established** — `cmd/fuse/loop_server_multiloop_test.go` builds a scripted
  `httptest.NewServer` and `t.Setenv("LLM_GATEWAY_URL", srv.URL)`; #55's acceptance tests reuse this
  (never Claude/Anthropic, project policy). The two-instance reconnect test stands up two `connect-go`
  servers over one durable store and toggles the client's re-open onto the second (in-test router).

`reconciled: true`. Auto-capture disabled this repo → the local-tree-staleness lesson is reported in
the run output for the learnings harvest, not minted as a stub.
