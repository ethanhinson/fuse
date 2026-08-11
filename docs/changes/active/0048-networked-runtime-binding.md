---
id: 48
slug: networked-runtime-binding
title: Networked binding over the Runtime seam — WS live observe + HTTP start/send/replay
status: implemented
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [46, 47]
related: [45, 46, 47, 53]
discovered_from: [45]
adrs: [32]
spec: docs/superpowers/specs/2026-08-11-networked-runtime-binding-design.md
plan: docs/superpowers/plans/2026-08-11-networked-runtime-binding.md
results: docs/results/2026-08-11-networked-runtime-binding-results.md
trivial: false
auto_groomable:
branch: feat/networked-runtime-binding
pr: https://github.com/ethanhinson/fuse/pull/51
blocked_by:
reconciled: true
claimed_at: 2026-08-11T02:49:50Z
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-networked-runtime-binding-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-networked-runtime-binding-design.md) |
| Plan | [2026-08-11-networked-runtime-binding.md](https://github.com/ethanhinson/fuse/blob/feat/networked-runtime-binding/docs/superpowers/plans/2026-08-11-networked-runtime-binding.md) |
| Results | [2026-08-11-networked-runtime-binding-results.md](https://github.com/ethanhinson/fuse/blob/feat/networked-runtime-binding/docs/results/2026-08-11-networked-runtime-binding-results.md) |
| PR | [#51](https://github.com/ethanhinson/fuse/pull/51) |
| ADRs | [ADR-0032](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0032-binding-3-websocket-session-http-replay-shared-dispatch.md) |
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

## Reconcile log

### 2026-08-11 — build reconcile (claim → plan)

Reconciled against `origin/main` (tip `d6d3e77`), the spec, related changes 45/46/47, and current code.
Design is **fully valid**; scope unchanged; no obsolescence. Open questions from the spec resolved:

- **Dependency #47 (PR #50) has MERGED into `origin/main`** — the durable EventStore + registry seam
  (`DurableStore`/`Registry` on `runtime.Deps`, selected via `selectDurableBackend` in
  `cmd/fuse/loop_server.go`) is present. The spec's "if 47 slips, 48 waits" contingency is closed:
  47 is `done`. Binding #3 resolves loops through the same durable seam the loop-server already uses.
- **Extraction target confirmed** — `internal/loopserver/server.go` couples dispatch to stdio via
  `enc *json.Encoder` / `dec *json.Decoder`, with the `encMu` shared-encoder mutex serializing
  responses against id-less `loop.event` notifications. `serveObserve` implements subscribe-before-replay
  + dedup-at-watermark (`ev.Seq <= last`) + gap detection (`ev.Seq > prev+1 && prev != 0`, ADR-0025
  drop-newest). This is exactly the discipline to preserve verbatim. **Structuring decision (open q1):**
  keep the extracted core inside `internal/loopserver` with a `transport` frame-read/write abstraction
  and `stdio.go` + `net.go` transports (over a new sibling package) — least churn, keeps binding #2
  behaviorally identical. Actual method params today are `from_seq` / `tenant` (not the spec's prose
  `from`/`tenant_id`) — the wire is inherited as-is.
- **Composition root reuse** — the new subcommand calls `buildLoopServerRuntimeDeps(...)` +
  `runtime.New(deps)` identically to `runLoopServer` (`cmd/fuse/loop_server.go`), then serves over
  WS/HTTP instead of `loopserver.NewServer(stdin, stdout, rt)`. Per-loop `BuildAgent` factory (change
  46) is inherited unchanged; N concurrent loops stay isolated.
- **Wire format (D5)** — `event.Event` (`internal/event/event.go`: `Seq uint64` → `json:"seq"`, full
  existing encoding) and `event.TenantID` (`string`) cross unchanged. No new envelope.
- **HTTP replay (open q2)** — maps directly to `rt.Attach(ctx, tenant, loopID, from) ([]event.Event, error)`.
  Stateless `GET /loops/{id}/events?from=<seq>` (+ optional tenant carrier), returning the durable
  history JSON array. No `loop.start`/`loop.send` on HTTP (D3).
- **Dependency (D6)** — `github.com/coder/websocket` is NOT in `go.mod` (Go 1.26.5); latest v1.8.15 is
  fetchable from the proxy. Real new dep, as the spec anticipated.
- **Testing (open q3)** — the in-process WS client↔server test MUST drive live + replay concurrently
  (force an append into the subscribe→replay gap) per the `replay-live-handoff-dedup-at-watermark`
  learning; a sequential test cannot see the double-delivery. The WS client read pump must route id-less
  `loop.event` notification frames distinctly from id-keyed responses (`mcp-read-pumps-drop-inbound-notifications`
  learning). Live verification, per project policy, uses a cheap scripted `LLM_GATEWAY_URL` double —
  never Claude/Anthropic.
- **ADR (open q5)** — the binding-#3 transport decision (WS-full-session + thin-HTTP-replay, shared
  core, `coder/websocket`) is recorded at Step 6, relating to ADR-0028 (JSON-RPC-not-MCP) and the
  multi-loop / durable-store ADRs (0030/0031).

No follow-up work surfaced that would warrant a new change (`auto_capture` is disabled anyway).
