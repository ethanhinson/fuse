---
id: 33
slug: networked-binding-connect-protobuf-fuse-loop-v1
title: Networked binding transport = Connect/protobuf (fuse.loop.v1), replacing the JSON-over-WebSocket + HTTP-replay wire
status: Accepted
date: 2026-08-11
supersedes: [32]
reverses: []
relates_to: [28, 30, 31]
change: 55
---

## Context

Change #48 (ADR-0032) shipped the networked loop-control binding on JSON-over-WebSocket (coder/websocket) plus a thin stateless HTTP replay endpoint, reusing binding #2's `loop.*` JSON-RPC protocol and `event.Event`'s JSON encoding over a transport-agnostic dispatch core. That proved the `Runtime` seam is portable across a third transport, but it left the cross-language contract IMPLICIT: a TS/Python client and the Go server share no schema, so every non-Go SDK (starting with #50's TS SDK for the Wander browser testbed) must hand-track the envelope. The networked wire had zero consumers yet (the SDKs are unbuilt), so the JSON wire could be replaced outright with no transition period. The loops also run behind a reverse proxy / load balancer, so the transport must be resilient to reconnect-to-a-different-instance and idle-timeout of parked streams.

## Decision

Replace the JSON-over-WebSocket + HTTP-replay networked binding with a Connect (connectrpc.com) + protobuf transport, schema-first. Define the `loop.*` service and the `event.Event` message in a versioned protobuf package `fuse.loop.v1` and serve it with connect-go (over h2c), reusing the SAME policy-free `runtime.Runtime` seam and the reattach discipline (subscribe-before-replay + dedup-at-watermark `ev.Seq<=last` + gap markers).

- `StartLoop` and `Send` are unary RPCs; `Observe` is a server-streaming RPC (history-then-live) that SUBSUMES the old Attach/HTTP-replay endpoint via `Observe(from_seq)` — there is no separate HTTP replay route.
- Only unary + server-streaming are used — NO bidi (Connect bidi does not work in browsers; the protocol never needs it). This makes the wire browser-native via connect-es with no Envoy/grpc-gateway proxy.
- protobuf is the IDL (mandatory with Connect, mature multi-language). Stubs are generated with buf and committed (generate-and-commit): Go stubs in `internal/loopwire/v1` (+`loopv1connect`), TS stubs in `proto/gen/ts`, so #50's Go + TS SDKs generate from one authoritative contract.
- Go maps the proto `Event` at the transport edge (package `internal/loopconnect`) to keep `internal/event` transport-free.
- `tenant_id` remains present-but-unenforced, now a typed proto request field (identity is #49); the trust model is unchanged, only the encoding.
- Ingress resilience is a first-class software-architecture requirement: a dropped `Observe` stream is normal and a reconnect may land on a DIFFERENT instance and must still replay correctly over the durable cross-instance store (#47) — no sticky sessions assumed; parked/idle streams survive via a server-sent keepalive frame (below a common gateway idle default) or cleanly re-establish from `from_seq`; forwarded-header (`X-Forwarded-Proto`) awareness; deadline/retry safety through an intermediary with per-attempt request-body rewind. Every post-open stream error is a clean reconnect, never a server fault.
- The #48 coder/websocket binding, its `ServeWS` transport, and the HTTP replay handler are REMOVED (safe: zero consumers). The stdio binding #2 (`internal/loopserver`) and the `Runtime` seam are untouched.

## Consequences

Enables: one protobuf schema is the single source of truth for the `loop.*` wire; every SDK (Go, TS, later Python/mobile) generates its client from it with no hand-tracked envelope or drift; the browser reaches the wire natively via connect-es (no proxy), so the Wander testbed exercises start/drive/observe/replay end-to-end; #48's transport-agnostic-dispatch-core investment pays off as intended (a third transport under one seam). Costs / gives up: a protobuf toolchain enters the repo (buf + connect-go/connect-es codegen, generate-and-commit); the JSON WS/HTTP binding and the coder/websocket dependency are retired (a real deletion justified by zero consumers); reconnect/replay is re-proven over Connect streaming rather than WS; bidi is off the table in the browser (accepted — the protocol never uses it). Relates to ADR-0028 (reuse-proven-framing/own-the-dispatch spirit), ADR-0030 (the policy-free multi-loop seam this transport is proven portable against — now a THIRD transport), and ADR-0031 (the durable/distributed event store the different-instance reconnect relies on).
