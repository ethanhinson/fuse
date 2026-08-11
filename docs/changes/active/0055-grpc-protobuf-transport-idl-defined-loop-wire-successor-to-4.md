---
id: 55
slug: grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4
title: Connect/protobuf transport — IDL-defined loop.* wire, successor to #48
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: []
related: [46, 47, 48, 50]
discovered_from: [50]
adrs: [32]
spec: docs/superpowers/specs/2026-08-11-grpc-connect-transport-design.md
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
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-grpc-connect-transport-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-grpc-connect-transport-design.md) |
| ADRs | [ADR-0032](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0032-binding-3-websocket-session-http-replay-shared-dispatch.md) |
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
