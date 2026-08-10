---
id: 43
slug: runtime-eventstore-seam
title: Runtime EventStore seam — typed, pluggable, introspectable loop event stream
status: in-progress
priority: high
type: feat
created: 2026-08-09
updated: 2026-08-10
depends_on: []
related: [23, 24, 30, 36, 42]
discovered_from: []
adrs: [16, 17, 18, 19, 20]
spec: docs/superpowers/specs/0043-runtime-eventstore-seam.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/runtime-eventstore-seam
pr:
blocked_by:
claimed_at: 2026-08-10T04:55:11Z
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0043-runtime-eventstore-seam.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0043-runtime-eventstore-seam.md) |
| ADRs | [ADR-0016](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0016-subagent-spawn-tree-runtime.md), [ADR-0017](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0017-segment-store-fssink-subpackage-split.md), [ADR-0018](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0018-per-session-directory-layout-flat-log-read-compat.md), [ADR-0019](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0019-process-global-segment-sink-holder.md), [ADR-0020](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0020-born-compressed-non-destructive-segment-store.md) |
<!-- docket:artifacts:end -->

## Why

Fuse is evolving toward a portable "agentic loop runtime" — a fan-out + model-mesh loop
engine you can run as a process and hook into anything (a chat widget, a mobile app,
Claude/Codex/Cursor) through a stable, pluggable interface. The load-bearing bet is that
the engine sits behind a small seam and every integration is a *binding* over one event
stream, so the platform is emergent rather than built.

Today the loop has no such stream. Its activity leaks out through three unrelated,
partial channels: a **write-only** forensic session log (`session/log.go`, coarse
per-node lifecycle entries, no reader, no subscribers), the **segment store** (gzip
archival of summarized-away context — a recovery slice, not live activity), and
**internal channels** (`SpawnDone` on `AgentHandle`, in-process and point-to-point). No
consumer can subscribe to "everything the loop is doing" as one typed, introspectable
stream, and nothing produces a durable, replayable record a later change could resume
from.

This is the **first of three changes** toward that seam, and deliberately the additive,
de-risking one everything else lands on:

1. **This change** — a typed `Event`, a pluggable `EventStore` (`Append`/`Subscribe`/
   `Replay`), a JSONL impl, and loop emission at every state transition. The session log
   is re-expressed as a *consumer* of the stream.
2. **`spawn-handle-async`** (next) — `SpawnFunc` goes from synchronous `(string, error)`
   to handle-returning; a child's result arrives as an event on this stream.
3. **`runtime-interface-and-binding`** — extract the named `Runtime` interface and stand
   up a second loop-control binding to prove the seam is policy-free.

Getting the stream right now is what makes durability (reattach/resume) and every future
binding fall out as filters over one source of truth, instead of bespoke integrations.

## What changes

Introduce a typed `Event` (a discriminated union over every loop state transition — turn,
model call, streaming token delta, tool call/result, subagent lifecycle, context
summarization, loop-detector trip, error) and an agent-free `EventStore` interface
(`Append` / `Subscribe` / `Replay`). Ship a JSONL-backed impl in the per-session dir and a
process-global lock-guarded holder for injection — mirroring the existing segment-sink
architecture exactly (agent-free interface pkg → fs impl subpackage → global holder →
post-`New` setter → best-effort call from the loop; ADR-0017/0018/0019/0020). The loop
emits an event at every transition via explicit, greppable `Append` calls. `Event` becomes
the **canonical** record and the forensic session log is re-expressed as a *consumer* that
projects the existing `session.jsonl` format, so every current reader is byte-unaffected.

Coverage target is a fully robust, introspectable stream: **full payloads** on every event
and **streaming token deltas** at the model boundary. To keep the de-risking property, the
build stages this: Stage A is the fully additive core (schema, store, holder, full-payload
*boundary* events, log-as-consumer — no adapter changes, lands green); Stage B adds a
streaming path to `Completer` and both adapters and emits `model.delta` events — the only
non-additive part, fenced so it can slip to its own change without blocking the seam.

## Out of scope

- **Changing the `SpawnFunc` signature** — it stays `(string, error)`; the handle-returning
  contract is the next change (`spawn-handle-async`). This change adds subagent *lifecycle
  events* only.
- **Touching the tool-call MCP server** (`cmd/fuse/mcp_server.go`). The loop-control binding
  is change 3 and is a new, higher surface, not a modification of the tool-call one.
- **Implementing reattach/resume.** `Replay(from)` exists as the durable foundation, but
  wiring it to a restarted process is a later change.
- **A networked EventStore backend.** JSONL only; the interface leaves room, ADR-0017-style.
- **Deleting the now-redundant direct `Logger.Write` call sites.** The projected log runs
  alongside them transiently (verified identical); removing the dead writes is a trivial
  follow-up so this PR stays green-by-construction.

## Open questions

- **Segment-store overlap (must resolve in planning).** Full-payload events put the complete
  transcript in `events.jsonl`, overlapping the segment store's gzip-archived pre-summary
  messages (ADR-0020). Resolve deliberately: either events become the single complete record
  and segments a derived compressed view, or events reference segment/message storage for
  heavy content and stay lean. (`related: [30]`.)
- **`Seq` allocation across the spawn tree** — per-session-global (single total order, needs
  a process-wide monotonic counter) vs per-node (simpler, consumer merges). Lean
  per-session-global for a clean `Replay` cursor.
- **Subscriber back-pressure policy** — must never block the loop or wedge a scheduler slot
  (ADR-0016). Lean bounded buffer + drop-newest-with-a-gap-marker over silent drop.
- **`Observe` forward-compatibility** — this change has no `LoopHandle` yet (change 3);
  `Observe` is session-scoped `Subscribe()`. Confirm it narrows cleanly to a per-loop handle
  later.
