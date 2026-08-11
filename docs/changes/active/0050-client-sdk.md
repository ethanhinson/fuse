---
id: 50
slug: client-sdk
title: Client SDK — Runtime-parity Go + TS/JS libraries, same API local-or-remote
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [48, 49, 55]
related: [45, 48, 49, 55]
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
versioned client wire envelope) to this change.

The **Wander** demo app is a browser (TS/JS) client — it is the concrete reason #50 ships a **JS
SDK alongside the Go one**, and the reason the cross-language wire contract is in scope now.

## What changes

**Two SDKs** presenting the same **Runtime-parity** surface — method surface **identical to the
`Runtime` seam** (`StartLoop` / `Send` / `Observe` / `Attach`, keyed on `loopID`):

- **Go SDK** — local in-process backend **and** remote WS/HTTP backend, one constructor switch; both
  ship. The literal "same API local-or-remote" thesis.
- **TS/JS SDK** — **remote-only** (a browser has no in-process Go `Runtime`), full Runtime-parity over
  change 55's Connect transport, so the **Wander** browser app can start, drive, observe, and replay
  loops.

Both live in **this repo**: the TS SDK is a **monorepo npm workspace** (e.g. `sdk/ts`) and **Wander is
a sibling workspace package** depending on it — chosen because the proto, the Go SDK, and the TS SDK
co-evolve in lockstep right now, so a monorepo makes each proto→Go→TS→Wander change atomic and keeps
change 55's generated-stub codegen trivial; Wander still consumes the SDK through its package boundary
(exercising the real surface, not relative imports). Extraction to a standalone repo is a later option
once the wire stabilizes; a git submodule was rejected as painful for the current lockstep
co-development.

See the linked spec for the full design; at proposal altitude:

- **Versioned wire envelope comes from change 55.** A TS client and a Go server don't share types, so
  the `loop.*` envelope + `event.Event` shape must be a **versioned, schema-first contract**. That
  contract is now owned by **change 55** (the gRPC/protobuf transport, successor to change 48,
  superseding ADR-0032) — #50 **depends on 55** and generates both SDKs' clients from 55's IDL,
  rather than hand-generating TS types from Go structs over 48's JSON wire.
- **Runtime-parity, not "transparent."** The network is not invisible: both SDKs **surface** the
  event stream, the replay cursor (last `event.Seq`), gap/reconnect, and the **explicit
  park/completion event** as first-class — a client cannot infer "done" or "safe to reconnect" from
  the *shape* of a persistent loop's stream.
- **Remote client over change 55's Connect wire.** The Go SDK's remote backend and the TS SDK both
  drive change 55's Connect/protobuf transport, generating their stubs (`connect-go` / `connect-es`)
  from its proto. The reconnect discipline (subscribe-before-replay + dedup-at-watermark + gap-driven
  re-observe) is preserved over 55's server-streaming; the TS SDK reaches it in-browser via
  `connect-es` with **no proxy** (55's settled browser path). (Change 48's WS/HTTP client is superseded
  by 55, not extracted.)
- **Credential seam, pass-through now.** Each constructor takes an identity/tenant seam that forwards
  `tenant_id` present-but-unenforced today (matching change 48) and becomes the real auth carrier when
  change 49 lands — so 49 adds no breaking change to either SDK surface.

## Out of scope

- A **Python (or mobile-native) SDK** — a later change; #50 ships Go + TS/JS. The versioned envelope
  built here is what makes those cheap later.
- The **auth mechanism** (tokens / mTLS / OIDC) and enforcement — change 49; this change defines only
  the credential *seam*, in both languages.
- The **Wander app itself** — #50 ships the SDK Wander consumes, not Wander. Wander is the motivating
  consumer and acceptance target, not a deliverable here.
- Any **change to the `Runtime` seam** — the SDKs are clients *over* the seam.
- **Observability emission** (OTEL / `/metrics`) — change 51.
- **TLS / deployment topology / load-balancing** — operational concerns beneath change 48's transport.

## Open questions

- Go package home/name (`sdk/` vs `pkg/fusesdk`) — plan-time; must be importable from outside the
  module, never under `internal/`. **TS package home is decided:** a **monorepo npm workspace** in this
  repo (e.g. `sdk/ts`), with Wander as a sibling workspace package consuming it — see *What changes*.
  Bundler + publish target remain plan-time.
- How the SDKs consume change 55's IDL (generated stubs, package boundaries) and the browser path 55
  settles on — re-validated at the build reconcile pass once 55 is designed.
- Exact completion-signal API shape in each language (typed event on the stream, a handle await, or
  both) — the requirement (completion observable without stream-shape inference) is fixed; the surface
  is a plan-time call.
- How the credential seam reconciles with change 49's chosen auth mechanism, and how the browser SDK
  supplies its credential (Wander's token flow) — re-validated at the build reconcile pass.
