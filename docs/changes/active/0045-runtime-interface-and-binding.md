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
claimed_at: 2026-08-10T17:16:10Z
reconciled: true
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

## Reconcile log

### 2026-08-10 — reconciled against current code (implementer)

Re-read the spec, the five cited ADRs (16, 22, 24, 25, 26), and current code at the
integration-branch tip. **The design holds — no obsolescence, no fundamental invalidation, no
scope change.** Dependency 0044 is merged/archived (`2026-08-10-0044-spawn-handle-async.md`); its
ADR-0026 confirms `Spawner.Spawn → (agent.AgentHandle, error)` and that the future Runtime consumes
`agent.AgentHandle` directly (the `tools.SpawnHandle` interface is the tool-layer view only). All
spec anchors verified present in current code — no drift:

- **Three cmd-site builders confirmed**: one-shot in `cmd/fuse/main.go`, interactive in
  `cmd/fuse/shell.go`, research in `cmd/fuse/research_probe.go`; shared builders
  `buildAgentCore` + `spawnFuncFrom` live in `cmd/fuse/run.go`.
- **Exact post-New setter names** (spec's "named like" resolves to): `SetEventSink`,
  `SetNodeIdentity`, `EnableSummarization` (not `SetSummarization`), plus `SetStripSpawn`,
  `SetHumanInjector`, `SetExpects`. `agent.New(m, t, r, modelID, systemPrompt, maxTurns,
  maxTokens)`; loop entry `(*Agent).Run(ctx, history)`.
- **Loop handle key**: `(*AgentTree).RootID()` (`internal/agent/tree.go`).
- **EventStore**: interface in `internal/event` (`Subscribe() (<-chan Event, func())`,
  `Replay(from Seq) ([]Event, error)`), JSONL impl in `internal/event/fsstore` (note the
  subpackage — the impl is reached via the interface, not `internal/event` directly). Types
  `event.Seq` (uint64) and `event.Event`.
- **Process-global holders present** (ADR-0019 pattern, `cmd/fuse/run.go`): `setActiveEventStore` /
  `currentEventStore` (RW-mutex, no-op default when nil) and `setActiveSegmentSink` /
  `currentSegmentSink`. These are the D1 open-question target.
- **Human-bus IS implemented in code** (`internal/agent/humanmsg.go`): `HumanBus`
  (`NewHumanBus(tree)`, `Enqueue`/`Drain`/`OnNodeComplete`), wired via `SetHumanInjector` +
  `NewHumanInjector(nodeID, humanBus)`. So `Runtime.Send` has a concrete, existing target — the
  turn-boundary self-pull path. (Note: ADR-0022 metadata still reads `status: Proposed` / `change:
  0051` even though the substrate landed; that is a metadata lag, not a design gap — flag for a
  future ADR-status reconcile, not this change's job.)
- **`internal/runtime` does not exist yet** — clean slate, no partial work to fold in.
- **Tool-call MCP server** `cmd/fuse/mcp_server.go` present and untouched (out-of-scope invariant
  holds); stdio `mcp.NewServer(os.Stdin, os.Stdout, ...)` + `notifications/resources/updated`
  push pattern is the reusable model for binding #2's live tail. Subcommand dispatch is a
  `switch args[0]` in `main.go` (`mcp-server → runMCPServer`); `loop-server` hooks in there.
- **Tests**: `go test ./...`; race gate via `make test-race` (`go test -race ./...`).

**One constraint sharpened (folds into D1/planning, not a scope change).** ADR-0025 makes the
per-session-global Seq allocator assume **one process ⇒ one session** (ADR-0019); its own
Consequences say a multi-session-per-process store "would revisit Seq allocation." So the open
question "N concurrent loops per loop-server process" is *not free*: **interface designed for N
(loopID-keyed), in-process impl stays single-loop-per-process for this change**, with the holder
migration to instance state scoped carefully (a full de-globalization / multi-loop Seq allocator is
its own later change). This is the spec's existing lean, now backed by the ADR-0025 constraint.

Auto-capture is disabled (`AUTO_CAPTURE_ENABLED=false`); the ADR-0022-status-lag observation above
is reported in-prose only, not minted.
