---
id: 16
slug: subagent-spawn-tree-runtime
title: Subagent runtime is an append-only spawn tree with bounded depth and width and slot-yield deadlock avoidance
status: Accepted
date: 2026-08-08
supersedes: []
reverses: []
relates_to: [2, 7]
change: 12
---

## Context

This ADR is a **backfill**: change 0012 shipped the subagent runtime — the
substrate every later spawn feature builds on — without recording the
architectural decisions it baked in. ADR-0007 later declared the Scheduler the
single admission authority, and ADR-0002 documents research mode running *on* the
subagent runtime, but neither states how the runtime itself is shaped. A reader
of ADR-0007 finds a policy layer presupposing a data model, a depth/width regime,
and a nested-spawn deadlock fix that are described nowhere. This ADR captures
those decisions so the runtime's *why* is answerable, not archaeology.

The runtime must let one agent turn fan out into many child agents, running
concurrently, while staying observable (a TUI must render live tree state), bounded
(unbounded fan-out or recursion is a resource and cost hazard), and deadlock-free
(a parent that blocks awaiting its own child must not hold the only slot that child
needs to start).

## Decision

The subagent runtime is an **append-only spawn tree**.

- **Data model.** `AgentTree` holds `AgentNode`s (identity, parent link, label,
  model, status, depth, per-node token counters) and per-node `AgentEvent`s. Nodes
  and events are **append-only**: a node is never removed, only transitioned and
  finished, and subtree aggregates (counts, token sums) are monotonic. This makes
  concurrent snapshots for the TUI cheap and race-free, and makes the tree a
  faithful audit log of a turn.

- **Bounded depth.** `MaxDepth = 5` is a hard cap enforced synchronously at spawn
  time: a node at `MaxDepth` cannot spawn (its spawn tool is stripped). Depth
  bounds recursion — an agent spawning agents spawning agents — independent of
  width.

- **Bounded width.** `MaxConcurrentSpawns = 16` is a tree-global concurrency
  ceiling: a slot semaphore caps how many child agents run at once across the whole
  tree, not per-parent. Depth alone does not bound load when a single node fans out
  widely, so width is capped separately.

- **Slot-yield to avoid the depth-2 deadlock.** A parent that blocks awaiting a
  child it spawned must **yield its slot** (`YieldSlot`) while waiting and
  **reacquire** it (`UnyieldSlot`) when resumed, jumping the queue via a priority
  lane so it never blocks behind its own descendants. Without this, with more
  blocked parents than slots, every slot is held by a waiting parent and no child
  can ever start — the runtime freezes (captured as learning #12).

These are runtime invariants. The *policies* layered on top (global brake,
per-workflow pools, rate/quota) are ADR-0007's Scheduler concern; this ADR owns the
tree shape, the depth/width caps, and the yield discipline they operate against.

## Consequences

- **Observable by construction.** Append-only nodes/events let the TUI take
  lock-light snapshots and render live tree state without coordinating with the
  running agents; the tree doubles as a per-turn audit trail.
- **Bounded blast radius.** Depth and width caps put a ceiling on recursion and
  concurrency before any cost/rate policy is considered, so a runaway prompt cannot
  fan out without limit even absent a configured quota.
- **Nested spawns are safe.** Slot-yield makes parent-awaits-child a first-class,
  deadlock-free pattern, which is what makes workflows and fan-out research
  composable rather than freeze-prone.
- **Memory grows with a turn.** Append-only means a very wide/long turn's tree is
  retained in full for the turn's lifetime; this is an accepted trade for
  observability and audit, bounded in practice by the depth/width caps.
- **The caps are constants, not config.** `MaxDepth`/`MaxConcurrentSpawns` are
  compile-time defaults (the Scheduler layer adds configurable policy on top); a
  reader changing fan-out behavior tunes Scheduler policy, not these floors.
