---
id: 45
slug: runtime-interface-and-binding
title: Runtime interface + second binding — prove the platform boundary is emergent
status: in-progress
priority: high
type: feat
created: 2026-08-10
updated: 2026-08-10
depends_on: [44]
related: [22, 23, 36, 43, 44]
discovered_from: [43]
adrs: [16, 22, 24, 25, 26]
spec: docs/superpowers/specs/0045-runtime-interface-and-binding.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/runtime-interface-and-binding
pr:
blocked_by:
claimed_at: 2026-08-10T17:12:23Z
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0045-runtime-interface-and-binding.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0045-runtime-interface-and-binding.md) |
| ADRs | [ADR-0016](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0016-subagent-spawn-tree-runtime.md), [ADR-0022](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0022-human-message-bus-per-node-queue-async-router.md), [ADR-0024](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0024-eventstore-independent-of-segment-store.md), [ADR-0025](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0025-eventstore-ordering-backpressure.md), [ADR-0026](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0026-handle-returning-spawn-seam-agent-free-interface.md) |
<!-- docket:artifacts:end -->

## Why

This is the payoff of the Runtime-seam trilogy. Changes 0043 (typed event stream) and 0044
(handle-returning, location-transparent spawn) built the engine primitives a portable
"agentic loop runtime" needs — but nothing yet names the boundary they form, and only the
CLI drives them. The thesis — *the engine sits behind a small, policy-free interface, and
every integration (mobile app, website, Claude/Codex/Cursor) is just a binding over it* —
stays unproven until the interface is a real named type AND at least two distinct bindings
drive it. One binding always leaks (you shape the seam to it); two is the minimum that forces
it to stay policy-free.

Today the engine is reachable only as in-process Go calls threaded through three cmd-site
builders, each independently wiring `agent.New` + setters + `Agent.Run` / `Spawner.Spawn` /
`EventStore`. There is no `Runtime` type and no surface where anything other than the CLI can
start, feed, spawn into, or observe a loop. The existing MCP server is tool-call only (a
model using fuse's tools, not a client driving a fuse loop).

This is the **third and final** change of the trilogy, depending on 0044 (merged): `Spawn`
returns 0044's handle, and `Observe`/`Attach` read 0043's event stream that 0044 wired spawn
lifecycle onto.

## What changes

Extract a named `Runtime` interface — `StartLoop` / `Send` / `Spawn` / `Observe` / `Attach` —
over what already exists (StartLoop→agent.New+setters+Run, Send→the ADR-0022 human-bus,
Spawn→Spawner.Spawn, Observe→EventStore.Subscribe, Attach→EventStore.Replay), keyed by the
existing `tree.RootID()` as the addressable loop handle, in a new composition package
(`internal/runtime`). The interface names loop mechanics only — no renderer, TUI, approval
gate, or transport — that is what makes it policy-free.

**Migrate the CLI (binding #1)**: the three cmd-site builders stop calling the engine directly
and consume `Runtime` instead; the renderer/TUI/approval gate stay in the cmd layer wrapping
the binding, never passed into the seam. That re-expression is the load-bearing proof the seam
is policy-free.

**Binding #2**: a new `fuse loop-server` subcommand — a dedicated stdio MCP loop-control
surface (`loop.start` / `loop.send` / `loop.observe`) driving the identical `Runtime`, headless
(no gate/renderer/TUI). It is a pure adapter from MCP JSON-RPC to `Runtime`; its headlessness is
the proof. `loop.observe` supports **both** a live server-push tail (via EventStore.Subscribe /
MCP notifications) **and** replay catch-up from a cursor (via EventStore.Replay) — the durable
reattach story: a client disconnects, reconnects with its last Seq, and misses nothing.

## Out of scope

- **A networked spawn backend** — still in-process; the interface merely *permits* one later.
- **Modifying the existing tool-call MCP server** (`cmd/fuse/mcp_server.go`) — binding #2 is a
  **separate** `fuse loop-server` subcommand; the tool-call surface stays byte-untouched.
- **Auth / multi-tenancy** on the loop-server — single-tenant stdio, same trust model as the
  existing mcp-server; the many-users platform story is later work.
- **A TUI/renderer for binding #2** — the point is that the seam carries none.

## Open questions

- **Runtime impl vs process-global holders.** The event store/segment sink install via
  process-global holders today (ADR-0019 pattern). Should `Runtime` own them as instance state
  (needed if a server hosts >1 loop) or keep globals for this change and defer de-globalization
  to its own change? Lean: instance state, blast radius scoped carefully. Record an ADR for the
  Runtime seam + this decision.
- **Multiple concurrent loops per loop-server process** — loopID-keyed N loops (real platform
  story) vs one loop per process for this change. Lean: interface designed for N, impl 1-or-N as
  the holder decision allows.
- **`Send` through the human-bus** — confirm ADR-0022 turn-boundary injection maps cleanly for a
  headless binding, and how a message to an idle/finished loop is handled.
- **Approval gating in binding #2** — auto-approve (the "policy lives in the binding; this
  binding's policy is no gate" stance) vs a relay. Lean: auto-approve, documented as the
  binding's choice, not the seam's.
