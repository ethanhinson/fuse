---
slug: break-import-cycle-with-agent-free-subpackage
title: Break a read-side/write-side import cycle by splitting the agent-free schema+reader from the agent-dependent writer
hook: "When a tool reader and its concrete writer both need a shared domain package but the writer imports the caller (tools → x → agent → tools), keep the schema + reader agent-free in the base package and put the agent-dependent writer in a leaf subpackage imported only by the composition root (cmd/*)"
topics: [go, architecture, subagents, packages]
changes: [30]
created: 2026-08-09
updated: 2026-08-09
promotion_state: candidate
---

## Apply

A built-in tool that **reads** persisted domain data lives in `internal/tools`; the concrete
**writer** for that data must import `internal/agent`. If both the reader and the writer sit in
one shared package, you form the cycle `tools → <shared> → agent → tools` and the build breaks.

**Rule:** split the shared package by dependency weight, not by feature. Keep the **schema +
reader** (the part `internal/tools` needs) agent-free in the base package (`internal/segment`),
and put the **agent-dependent writer** in a leaf subpackage (`internal/segment/fssink`) that is
imported **only by the composition root** (`cmd/fuse`), never by `internal/tools`. The tool reads
through the agent-free base; the root wires the heavy writer in. A later refactor that collapses
the subpackage back reintroduces the cycle — the split is load-bearing, not cosmetic.

## War story
- 2026-08-09 (#30, PR #41) — Segment store: `segment_read` (in `internal/tools`) had to read
  segments while the concrete `FSSegmentSink` had to import `internal/agent`, forming
  `tools → segment → agent → tools`. Resolved by keeping `internal/segment` agent-free (schema +
  `LoadSegment`) and putting the agent-dependent sink in `internal/segment/fssink`, imported only
  by `cmd/fuse`. Recorded as ADR-0017.
