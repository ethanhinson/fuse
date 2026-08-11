---
id: 48
slug: networked-runtime-binding
title: Networked binding over the Runtime seam — WS live observe + HTTP start/send/replay
status: in-progress
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [46, 47]
related: [45, 46, 47]
discovered_from: [45]
adrs: []
spec: docs/superpowers/specs/2026-08-11-networked-runtime-binding-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/networked-runtime-binding
pr:
blocked_by:
reconciled: false
claimed_at: 2026-08-11T02:04:34Z
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-networked-runtime-binding-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-networked-runtime-binding-design.md) |
<!-- docket:artifacts:end -->

## Why

With the Runtime able to host N concurrent loops per process (change 46) and a loop's existence +
history made durable and cross-instance-reachable (change 47), the next hosting milestone is a
remote loop over the network: exposing the identical `Runtime` seam over a network transport so a
client on another machine can drive a loop — "attach to your running loop from your phone after the
server redeployed." This is the **third binding** over the seam — after the CLI (binding #1) and the
stdio `fuse loop-server` (binding #2) — and driving the *same* policy-free `Runtime` over a third
transport **with no seam change** is exactly the proof the boundary is portable across transports.

Binding #2 already speaks the `loop.*` JSON-RPC protocol (`loop.start` / `loop.send` /
`loop.observe`, with live `loop.event` push, gap markers, and subscribe-before-replay +
dedup-at-watermark reattach) — but only over `os.Stdin`/`os.Stdout`, so it is unreachable over the
network. Binding #3 reuses that proven protocol and moves it onto the wire.

## What changes

A networked binding exposing the identical `Runtime` as a **third transport over binding #2's
protocol** — not a new protocol. See the linked spec for the full design; at proposal altitude:

- **Reuse binding #2's `loop.*` JSON-RPC protocol + event framing**; extract a
  **transport-agnostic dispatch core** from `internal/loopserver` so stdio and network drive one
  implementation (binding #2 stays behaviorally identical, `internal/mcp` byte-untouched guard
  intact).
- **WebSocket carries the full session** — `loop.start` / `loop.send` / `loop.observe` + the
  server-push `loop.event` tail over one connection (binding #2's stdio protocol, now over WS).
  **HTTP** is a thin, **stateless** replay endpoint (`Attach(loop_id, from)`) for catch-up-only
  late joiners.
- **Reconnect is client-driven and the server is stateless**: the client tracks its last
  `event.Seq` and re-observes `from=<lastSeq>`; the inherited subscribe-before-replay +
  dedup-at-watermark path guarantees no loss / no dup; gap markers drive re-observe.
- **Wire format is inherited**: `event.Event`'s existing JSON encoding, `loop_id` (`tree.RootID()`)
  as the handle — no new envelope, no new handle type.
- **WS library:** `github.com/coder/websocket` (context-first, minimal deps).
- A new `fuse` subcommand serving the binding, wired through the *same* composition root that builds
  the multi-loop `Runtime`, hosting N concurrent loops over #0046's per-loop state and #0047's
  durable store + registry. `tenant_id` flows through present-but-unenforced (identity is #0049).

## Out of scope

- **Auth / multi-tenancy / tenant identity** — change 49, layered on top of this transport.
- **Any change to the `Runtime` seam** — this is purely a new binding.
- **REST-native `loop.start`/`loop.send` over HTTP** and a **versioned external-SDK wire envelope** —
  client-SDK ergonomics, change 50.
- **Observability emission** (OTEL / `/metrics`) — change 51.
- **TLS / deployment topology / cross-instance load-balancing** — operational concerns on top of the
  transport.
