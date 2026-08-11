---
id: 50
slug: client-sdk
title: Client SDK — thin-client library, same API local-or-remote
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [48, 49]
related: [45, 48, 49]
discovered_from: [45]
adrs: [26, 32]
spec: docs/superpowers/specs/2026-08-11-client-sdk-design.md
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
| Spec | [2026-08-11-client-sdk-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-client-sdk-design.md) |
| ADRs | [ADR-0026](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0026-handle-returning-spawn-seam-agent-free-interface.md), [ADR-0032](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0032-binding-3-websocket-session-http-replay-shared-dispatch.md) |
<!-- docket:artifacts:end -->

## Why

The end state is that the user's other apps are **thin network clients** over a hosted fuse
service. A client SDK gives those apps a small library to import that exposes the **same API
whether the runtime is local (in-process) or remote (over the wire)** — the same surface as the
`Runtime` seam, backed by the networked binding (change 48) and its auth (change 49) when remote.
This is what makes "any AI app can target fuse" concrete: import the SDK, get a loop, local or
hosted, identically. Change 48 explicitly deferred SDK ergonomics (a REST-native surface and a
client wire envelope) to this change.

## What changes

A **Go** SDK package — the first SDK — presenting a **Runtime-parity** client: its method surface is
**identical to the `Runtime` seam** (`StartLoop` / `Send` / `Spawn` / `Observe` / `Attach`, keyed on
`loopID`) whether it drives a **local** in-process `Runtime` or a **remote** one over change 48's
WS/HTTP wire. One constructor switch picks the backend; both ship in this change. See the linked spec
for the full design; at proposal altitude:

- **Runtime-parity, not "transparent."** One API, two backends. The word "transparent" is
  deliberately avoided — the network is not invisible: the SDK **surfaces** the event stream, the
  replay cursor (last `event.Seq`), gap/reconnect, and the **explicit park/completion event** as
  first-class, because a client cannot infer "this exchange is done" or "safe to reconnect here" from
  the *shape* of a persistent loop's event stream.
- **Extract change 48's WS/HTTP client** into the SDK as the single remote-backend implementation —
  one home for subscribe-before-replay + dedup-at-watermark + gap-driven re-observe (promoted from
  test-facing to library-grade).
- **Local backend** is a thin adapter over the in-process `Runtime` (no network); **remote backend**
  drives the wire. Both satisfy one internal backend interface; the public client is
  backend-agnostic.
- **Credential seam, pass-through now.** The constructor takes an identity/tenant seam that forwards
  `tenant_id` on the wire present-but-unenforced today (matching change 48) and becomes the real auth
  carrier when change 49 lands — so 49 adds no breaking change to the SDK surface.
- **Go-only ⇒ wire inherited as-is** from change 48 (`event.Event` JSON, `loop_id` handle, `loop.*`
  methods); no new cross-language envelope is minted here.

## Out of scope

- A **non-Go (TS / Python) SDK** and the **versioned language-neutral wire envelope** it forces — a
  later change; this SDK is Go-only and inherits change 48's wire.
- The **auth mechanism** (tokens / mTLS / OIDC) and enforcement — change 49; this change defines only
  the credential *seam*.
- Any **change to the `Runtime` seam** — the SDK is a client *over* the seam.
- **Observability emission** (OTEL / `/metrics`) — change 51.
- **TLS / deployment topology / load-balancing** — operational concerns beneath change 48's transport.

## Open questions

- Final package home/name (`sdk/` vs `pkg/fusesdk`) and public type/constructor names — resolved at
  plan time (must be importable from outside the module; never under `internal/`).
- Exact completion-signal API shape (typed event on the `Observe` channel, a handle await, or both) —
  the requirement (completion observable without stream-shape inference) is fixed; the surface is a
  plan-time call.
- How the credential seam reconciles with change 49's chosen auth mechanism — re-validated at the
  build reconcile pass.
