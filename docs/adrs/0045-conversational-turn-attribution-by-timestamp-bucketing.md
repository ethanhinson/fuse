---
id: 45
slug: conversational-turn-attribution-by-timestamp-bucketing
title: Conversational turns are attributed by timestamp bucketing, never by the event stream's Turn field
status: Accepted
date: 2026-08-16
supersedes: []
reverses: []
relates_to: []
change: 66
---

## Context

Changes 0053/0054 made the fuse shell's root loop a persistent multi-turn conversation: one
`loop_id`, park-at-turn-end, and `AgentTree.BeginTurn()` called once per human prompt from
`internal/tui/shell_model.go`.

The event stream already carries a field named `Turn`: `event.UserInputPayload.Turn`,
`TurnStartPayload.Turn` and `TurnEndPayload.Turn`, all stamped from `turn` in
`internal/agent/loop.go` (`for turn := 0; a.maxTurns <= 0 || turn < a.maxTurns; turn++`).

Change 0066's spec originally assumed the UI could join its per-turn marks to that field — it is the
obvious move, and the names match exactly. Reconcile established that the join is **unsound**:
`loop.go`'s `turn` counts the agent's **inner tool-loop iterations** — one conversational turn
routinely spans many of them — while `BeginTurn`/`EndTurn` are **conversational** boundaries driven
by human prompts. The two counters coincide only by accident. This is a genuine naming trap: two
different things are both called "turn" in this codebase, one on the wire and one in the UI/tree
layer.

Additionally, most event kinds carry no conversational turn index at all, so even a sound join would
only cover a fraction of the transcript.

## Decision

Conversational turns are attributed by **timestamp bucketing against per-turn marks on the root
agent node** — never by joining the event stream's `Turn` field.

- `agent.TurnMark{Turn, StartedAt, EndedAt, Prompt}` is appended to the root `AgentNode` by
  `BeginTurn`. Its `Turn` is a **UI-local, 1-based conversational ordinal**, deliberately NOT the
  wire field.
- Attribution of any timestamped item — an event's offset, a turn group's membership, a blackboard
  entry's bucket — is: **the latest `TurnMark` whose `StartedAt <= the item's timestamp`**.
- That rule is implemented **once**, as `turnIndexFor`/`turnStartFor` in
  `internal/tui/agents_model.go`; grouping, offsets and blackboard bucketing all route through that
  single helper so no second rule can fork.
- Consuming code MUST NOT read `event.UserInputPayload.Turn` / `TurnStartPayload.Turn` to mean a
  conversational turn.

## Consequences

**Enables.** Correct per-turn timing with no wire-format, durable-stream or `reconstruct.go` change.
This was the fix for event offsets rendering as large negatives (`[-24013.7s]` observed live, caused
by `BeginTurn` clobbering `root.StartedAt` while offsets were computed against it). It works
uniformly for every event kind. An empty `Turns` slice is the backward-compatible legacy/child-node
path.

**Costs.** Attribution is only as good as the marks' clock; an item timestamped before the first
mark buckets into turn 1 by convention. The root node must be the single owner of turn marks.

**Gives up.** The tempting exact join, and with it any ability to attribute events that arrive out
of timestamp order.

**Future-proofing.** If a genuine conversational-turn index is ever needed on the wire, it must be
introduced as a **new, differently-named field**. Reusing the existing `Turn` would silently
resurrect exactly this bug.
