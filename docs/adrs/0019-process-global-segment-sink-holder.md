---
id: 19
slug: process-global-segment-sink-holder
title: Process-global, lock-guarded segment sink holder as the sink-injection mechanism in cmd/fuse
status: Accepted
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [17, 18]
change: 30
---

## Context

The concrete filesystem `SegmentSink` for change #0030 must be injected into the agent's summarization path (via `EnableSummarization`). Threading a new sink parameter through the several agent-builder / child-spawn call sites in `cmd/fuse` would touch many signatures.

fuse already establishes a pattern for exactly this shape of session-scoped, process-wide resource: `tools.SetSpillDir` (`internal/tools/spill.go`) and `tools.SetSegmentsDir` (`internal/tools/segment_read.go`) are package-level holders guarded by a `sync.RWMutex`, set once at session startup and read on child-spawn goroutines. The design rests on the invariant that **one OS process serves exactly one interactive shell session**.

## Decision

Introduce a package-level `activeSegmentSink` holder in `cmd/fuse`, guarded by a `sync.RWMutex` (`setActiveSegmentSink` write-locks, `currentSegmentSink` read-locks), mirroring the existing `SetSpillDir` / `SetSegmentsDir` holders.

It is set **exactly once** — in the interactive shell path, after the agent tree exists so the root session id is known — and read by `installSummarizer` when building each agent. One-shot / probe / mcp paths never set it, so `currentSegmentSink()` returns nil and the agent falls back to the no-op sink (no archival).

The holder **MUST be lock-guarded** (not a bare package var), both for consistency with its siblings and to be safe under the child-spawn goroutines that read it. A code review of #0030 caught an initially-unsynchronized version and it was fixed to use the `RWMutex`.

## Consequences

- (+) Sink injection needs no new parameter threaded through the builder call sites; consistent with the two existing sibling holders.
- (+) The nil-default cleanly gives one-shot / probe / mcp the no-op sink for free.
- (-) Relies on the one-process-one-session invariant — a future multi-session-per-process design would break all three of these holders and would need a per-session context object instead. This ADR pins that invariant and the rule that these holders must be lock-guarded.

Relates to change #0030.
