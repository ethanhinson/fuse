---
id: 35
slug: sdk-local-backend-takes-prebuilt-runtime
title: Client SDK Go local backend takes a pre-built runtime.Runtime (not config-to-build)
status: Accepted
date: 2026-08-11
supersedes: []
reverses: []
relates_to: [26, 33]
change: 50
---

## Context

Change 0050 ships a Go SDK (`sdk/fuse`, at the module root, importable from outside the
module — deliberately NOT under `internal/`) presenting the Runtime-parity surface
(`StartLoop`/`Send`/`Observe`) over one of two backends chosen by the constructor: a REMOTE
backend over the `fuse.loop.v1` Connect wire (change 0055 / ADR-0033), and a LOCAL backend
over an in-process `runtime.Runtime`. The spec left open (open question) whether the local
backend should take a pre-built `runtime.Runtime` or the config needed to build one.

Building a `runtime.Runtime` requires the full composition-root wiring
(`buildLoopServerRuntimeDeps`: tool registries, per-loop `BuildAgent` factory, blackboard,
spawner closures, durable-store selection, model registry) — all of which lives in
`cmd/fuse`, which is `package main` and therefore NOT importable by any library. Pulling
that wiring into an importable SDK package would drag the entire agent engine, tools,
renderer-adjacent, and CLI-config surface into `sdk/fuse`, defeating the point of a small
importable client and creating import-direction problems (learning
break-import-cycle-with-agent-free-subpackage: `runtime` is imported ONLY by the
composition root).

## Decision

The Go SDK local backend constructor is
`NewLocal(rt runtime.Runtime, creds Credentials, opts ...Option) *Client` — it takes a
PRE-BUILT `runtime.Runtime` the caller already constructed through its own composition, and
forwards each SDK call to the seam (threading `creds.Tenant`). "Local" means "you already
have an in-process runtime; the SDK gives it the Runtime-parity surface." The SDK never
builds a runtime, never imports `cmd/fuse`, and never re-implements the composition root.

The remote backend, by contrast, needs only a base URL + credentials because the server
owns the composition. Reconnect on the local backend maps the wire's `Observe(fromSeq)` to
the seam's subscribe-before-replay + `Attach(fromSeq)` + dedup-at-watermark discipline
(ported from `internal/loopconnect/observe.go`), so local and remote present identical
no-loss/no-dup semantics.

## Consequences

- **Enables:** `sdk/fuse` stays a SMALL, importable client with a clean dependency
  footprint — it imports `internal/runtime` + `internal/event` (for the public `Event`
  alias) + the generated `loopv1`/`loopv1connect` stubs, nothing heavier. The composition
  root stays the sole builder of a runtime (no duplicate wiring to drift).
- **Costs / gives up:** the local backend is NOT a turnkey "give me a runtime from config"
  — the embedding app must build the runtime itself (as `cmd/fuse` does). That is the
  honest boundary: composition is the app's job, not the client library's. An app that
  wants a batteries-included builder would need a separate, heavier convenience package
  outside `sdk/fuse` (explicitly out of scope for 0050).
