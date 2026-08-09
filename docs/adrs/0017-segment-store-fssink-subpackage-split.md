---
id: 17
slug: segment-store-fssink-subpackage-split
title: Split the segment store into an agent-free internal/segment package and an agent-dependent internal/segment/fssink subpackage to break an import cycle
status: Accepted
date: 2026-08-09
supersedes: []
reverses: []
relates_to: []
change: 30
---

## Context

Change #0030 implements the concrete filesystem `SegmentSink` for fuse's context
summarization. Two consumers pull the segment store in opposite directions:

- The `segment_read` built-in tool lives in `internal/tools` and must read segment
  files, so `internal/tools` must import the segment schema + reader code.
- The concrete sink (`FSSegmentSink`) implements the `agent.SegmentSink` interface, so
  it must import `internal/agent`. But `internal/agent`'s tool-execution path
  transitively depends on `internal/tools`.

Putting both the reader and the sink in a single `internal/segment` package would
therefore create the cycle:

```
internal/tools -> internal/segment -> internal/agent -> internal/tools
```

## Decision

Keep `internal/segment` **agent-free** — it holds only the segment file/index schema,
rendering, and the reader, which is safe for `internal/tools` to import. Put the
agent-dependent concrete sink in a separate subpackage `internal/segment/fssink`, which
imports both `internal/segment` and `internal/agent` and is itself imported **only** by
`cmd/fuse` (the composition root that wires the sink into the agent via
`EnableSummarization`).

The rule: schema + reader are a **leaf** package that anything (including
`internal/tools`) may import; the sink lives one layer out where it can see the agent
interface; the composition root owns the wiring. This preserves the ADR-0012-style
boundary and keeps the dependency graph acyclic.

## Consequences

- (+) No import cycle: `segment_read` can import the reader freely; the sink stays where
  it can see the `agent.SegmentSink` interface; the composition root (`cmd/fuse`) owns
  the wiring.
- (+) Clear layering — schema/reader (leaf) versus sink (depends on agent).
- (-) An extra subpackage. A future refactor that naively collapses `fssink` back into
  `internal/segment` would reintroduce the cycle — this ADR is the guard against that.
- Relates to change #0030 (this change) and to change #0027, which defined the
  `SegmentSink` seam.
