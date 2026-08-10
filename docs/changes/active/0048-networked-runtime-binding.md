---
id: 48
slug: networked-runtime-binding
title: Networked binding over the Runtime seam — WS live observe + HTTP start/send/replay
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-10
depends_on: [46]
related: [45, 46, 47]
discovered_from: [45]
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

With the Runtime able to host N concurrent loops per process (change 46), the next hosting
milestone is (1) a remote loop over the network: exposing the identical `Runtime` seam over a
network transport so a client can drive a loop from another machine. This is a **third binding**
over the seam — after the CLI and the stdio loop-server — and driving the *same* policy-free
`Runtime` with no seam change is exactly the proof the boundary is portable across transports.
Needs brainstorming: the transport split (WS for live push, HTTP for request/response), framing,
and connection lifecycle.

## What changes

To be designed during grooming. At a sketch: a networked binding exposing the identical `Runtime`
— WebSocket for live `loop.observe` server-push and HTTP for `loop.start` / `loop.send` / replay
— as a third binding over the seam, with no change to the `Runtime` interface itself.

## Out of scope

- Auth / multi-tenancy — that is change 49, layered on top of this transport.
- Any change to the `Runtime` seam — this is purely a new binding.

## Open questions

- Transport split (WS vs HTTP) boundaries and framing for the event stream.
- Connection lifecycle, reconnect + replay-from-cursor over the wire.
- Serialization of `event.Event` / loop handles across the network.
