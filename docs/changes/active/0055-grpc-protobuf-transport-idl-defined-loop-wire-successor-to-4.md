---
id: 55
slug: grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4
title: gRPC/protobuf transport — IDL-defined loop.* wire, successor to #48
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: []
related: []
discovered_from: [50]
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

Change 48 (done, ADR-0032) pinned the networked binding's wire as JSON-over-WebSocket
(`coder/websocket`) + JSON-over-HTTP, reusing binding #2's `loop.*` JSON-RPC protocol and
`event.Event`'s existing JSON encoding. That was the right call to prove the transport quickly, but
it leaves the cross-language contract implicit: a TS/Python client and the Go server share no schema,
so every non-Go SDK (starting with the TS SDK in change 50) must hand-track the envelope or generate
types from the Go structs.

A schema-first IDL fixes that at the root: define the `loop.*` protocol and `event.Event` shape once
in **protobuf** and let a **gRPC** transport carry it, so every binding (Go, TS, future Python/mobile)
generates its client from one authoritative contract with versioning built in. This is the classic
answer to "many-language clients over one wire" — deferred at ADR-0032 only because #48 needed to
ship. This change reopens that decision deliberately, as a **successor to #48 that supersedes
ADR-0032**.

## What changes

To be designed during grooming. At a sketch: define the `loop.*` protocol + `event.Event` in a
protobuf IDL as the source of truth, and stand up a gRPC transport for the networked binding —
superseding ADR-0032's JSON-over-WS/HTTP — while preserving the existing `loop.start` / `loop.send` /
`loop.observe` semantics, the server-push `loop.event` tail, gap markers, and subscribe-before-replay
+ dedup-at-watermark reattach. The shared dispatch core (extracted in #48) should be reusable under
the new transport with minimal churn.

## Out of scope

To be defined during grooming. Auth/identity is change 49; the SDKs that consume this wire are change
50; observability is change 51.

## Open questions

- gRPC vs. keeping WebSocket framing but swapping the on-wire encoding to protobuf — how much of
  ADR-0032's transport is actually replaced.
- **Browser reach:** gRPC has no native browser support; does Wander's TS SDK (change 50) go through
  grpc-web + a proxy (Envoy/grpc-gateway), or does the browser path keep a WS/JSON bridge while
  server-to-server uses gRPC? This directly gates change 50's TS SDK.
- Migration: #48's JSON transport and its in-process client — replace outright, or run both wires
  during a transition?
- Streaming model fit: server-push `loop.event` over gRPC server-streaming vs. the current
  long-lived WS connection, including reconnect/replay semantics.
- protobuf vs. Cap'n Proto as the IDL, and where the generated Go/TS types live.
