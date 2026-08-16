<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0066 — Agents tab & blackboard — turn-aware multiturn UI (collapsible turn groups + per-turn timing)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0066-agents-tab-multiturn-turn-groups.md)**
<!-- docket:backlink:end -->

# Agents tab & blackboard — turn-aware multiturn UI — design

Change: [#0066](../../changes/active/0066-agents-tab-multiturn-turn-groups.md) ·
Discovered from #53 · Related #12, #53, #54 · ADRs consulted: none

## Problem

Change #53 gave the shell a **persistent conversational loop**: one `loop_id` carries a
multi-turn chat, the root loop *parks* at turn-end instead of returning, and each new user
prompt calls `AgentTree.BeginTurn()` to start another turn on the *same* root node. The
agents tab and blackboard views were built for the single-turn spawn-tree world of #12 and
never caught up. Two concrete defects result, both observed live (screenshots, 2026-08-16):

1. **No turn separation.** The root node's detail pane renders one flat, ever-growing event
   list. A second turn's events are appended after the first with no header, divider, or
   collapse — the operator cannot tell where turn 1 ends and turn 2 begins, and the pane
   grows without bound.

2. **Negative event timestamps.** The detail pane computes each event's `[Ns]` offset as
   `evt.TS.Sub(n.StartedAt)` (`internal/tui/agents_model.go:1120`, `:1240`). `BeginTurn()`
   **clobbers** `root.StartedAt = time.Now()` on every new turn
   (`internal/agent/tree.go:337`). So the moment turn 2 starts, every turn-1 event — whose
   `TS` is now *earlier* than the reset `StartedAt` — renders a large negative offset
   (`[-24013.7s]` in the screenshot). The timings are not merely ugly; they are wrong for
   every event outside the current turn.

The **blackboard view** shares the same root loop and the same single-clock assumption, so
it needs the analogous turn-aware treatment (decision: consistent mental model across both
views).

### Root cause

The root `AgentNode` has **no notion of turns**. It carries a single mutable clock
(`StartedAt`) that `BeginTurn()` overwrites, and a single flat `Events` slice with no
per-turn boundary. The event stream *already* contains the delimiter — `KindUserInput`
events carry a `Turn:` index (`internal/agent/loop.go:420`,
`event.UserInputPayload{Turn, Content}`) — but nothing in the node model or the UI keys on
it. Fix the node model to be turn-aware; the UI then reads that model.

## Goals

- Each event's rendered offset is relative to **its own turn's start** — never negative,
  always the true in-turn elapsed.
- The root's detail transcript is segmented into **collapsible per-turn groups**: prior
  turns collapse to a one-line header (turn #, prompt preview, duration); the current
  (in-flight or most recent) turn stays expanded. `enter` toggles a turn group.
- The root headline elapsed timer (tree top-right) reads the **current turn's** duration,
  preserving today's `BeginTurn` intent ("measures the turn, not the session").
- The **blackboard** view is turn-aware in the same shape: writer entries are grouped /
  divided by turn and any per-turn timing is correct.
- Child (spawned) agents are unaffected — they are single-turn by construction and keep
  today's rendering exactly.

## Non-goals

- No change to the event stream wire format, the durable stream, or `reconstruct.go`. The
  `Turn` index already exists; we consume it, we do not add to the protocol.
- No change to child-agent (subagent) rendering, the segment view, or the scheduler.
- No persistence of collapse/expand state across sessions — it is ephemeral view state.
- No new keybindings beyond reusing `enter` for turn-group toggle in the root detail pane.

## Design

### Data model — per-turn marks on the node (decision: on the node, not derived)

Stop clobbering the single clock. Add a per-turn record to `AgentNode` / `NodeView`
(`internal/agent/tree.go`):

```go
type TurnMark struct {
    Turn      int       // matches event.UserInputPayload.Turn
    StartedAt time.Time
    EndedAt   time.Time // zero while the turn is in flight
}
```

- `AgentNode.Turns []TurnMark` (snapshotted into `NodeView` like `Events`).
- `BeginTurn()` **appends** a new `TurnMark{Turn: n, StartedAt: now}` and sets its `EndedAt`
  zero, instead of overwriting `StartedAt`. It still sets `Status = StatusRunning` and emits.
  The root's overall `StartedAt` becomes the **first** turn's start and is set once (first
  `BeginTurn`), never rewritten — so anything that reads `StartedAt` as "when did this node
  begin" stays correct.
- `EndTurn(failed)` stamps the **current** (last) `TurnMark.EndedAt` and freezes status.
- The turn index `n` for `BeginTurn` comes from the same counter the loop uses for
  `UserInputPayload.Turn`; confirm the source at build time (`internal/agent/loop.go`
  passes it, or the tree increments — reconcile which is authoritative so the mark's `Turn`
  and the event's `Turn` are guaranteed equal). This equality is the join key the UI relies
  on; a build-time test must pin it.

Only the **root** node accumulates turns. Child nodes never call `BeginTurn`, so their
`Turns` slice stays empty and every consumer treats "empty Turns" as the single-turn legacy
case — this is the backward-compat and child-node path in one predicate.

### Per-event offset resolution

A small helper resolves an event's turn start:

```
turnStartFor(n NodeView, evt) time.Time:
    if len(n.Turns) == 0 { return n.StartedAt }          // legacy / child: unchanged
    find the TurnMark whose Turn == evt.Turn (join on the index)
    fallback: the latest TurnMark whose StartedAt <= evt.TS
    final fallback: n.StartedAt
```

The detail renderer (`renderEventLines`, `agents_model.go:1234`, and the expanded-event
title at `:1120`) uses `evt.TS.Sub(turnStartFor(n, evt))` instead of
`evt.TS.Sub(n.StartedAt)`. Offsets become correct and non-negative for every turn.

> Requires that events carry (or can be attributed to) a turn index. `KindUserInput` has
> `Turn`; most other events do not carry one on the wire. Resolve at build time by the
> **`StartedAt <= evt.TS` bucketing** fallback above (attribute each event to the latest
> turn that had started by its timestamp) — that needs no wire change and is robust. The
> `evt.Turn` exact-join is an optimization used only where the field exists.

### Turn grouping in the detail pane (collapsible)

`renderEventLines` for the **root** node changes from a flat loop to a grouped render:

- Bucket the (already filtered) events into turns by the same `StartedAt <= evt.TS`
  bucketing. Events before the first `TurnMark` (rare: pre-first-prompt) form an implicit
  "turn 0 / setup" bucket.
- For each turn render a **header row**: `turn N · "<prompt preview>" · <duration>` where
  the prompt preview is the turn's `KindUserInput` content (truncated) and duration is
  `EndedAt-StartedAt` (or "running" if in flight).
- **Current turn** (last) renders expanded (its event rows below the header). **Prior
  turns** render collapsed (header only) by default.
- Ephemeral `collapsed map[int]bool` (or `expandedTurn`) view state on `AgentsModel`;
  `enter` on a header toggles that turn. Selection/scroll math (`m.eventSel`,
  `eventScroll`) must account for collapsed turns contributing one line each — extend the
  visible-rows model that `renderEventLines` and the wheel/scroll handlers already use.
- Child nodes keep the flat render (empty `Turns` ⇒ old path). Guard the grouped path
  behind `len(n.Turns) > 1` so a single-turn root is visually identical to today.

### Root headline timer

`nodeElapsed` (`agents_model.go:1409`) for the root shows the **current turn's** elapsed:
`now - lastTurn.StartedAt` while running, `lastTurn.EndedAt - lastTurn.StartedAt` when
done. Collapsed prior-turn headers each show their own `TurnMark` duration. Child nodes and
legacy (empty `Turns`) keep `EndedAt-StartedAt`/`now-StartedAt` on the whole node.

### Blackboard — same consideration

The blackboard view (`internal/tui/agents_model.go` blackboard handlers +
`blackboard_*` renderers) renders writer-group entries for the root loop. Apply the
analogous treatment:

- Attribute each blackboard entry to a turn by the same timestamp bucketing (blackboard
  entries carry a write timestamp; confirm the field at build time).
- Insert a **turn divider / header** at each turn boundary in the blackboard body, and make
  any per-entry relative timing use the entry's turn start (fixing the same negative-offset
  class if present). Collapse of prior turns is desirable for consistency but is the lower
  bar — at minimum the blackboard must show correct timings and clear turn separation.
- `blackboardGroupStarts` (`:434`) and the blackboard scroll model already track
  body-line group offsets; extend them to include turn headers so scroll/selection stays
  correct.

## Testing strategy

- **Node model unit tests** (`internal/agent/tree_test.go`): `BeginTurn` appends a
  `TurnMark` and does **not** rewrite an existing `StartedAt`; `EndTurn` stamps the current
  mark; the mark's `Turn` equals the `KindUserInput.Turn` for that turn (pin the join key).
- **Timing regression test** (`internal/tui/agents_model` test): a two-turn root whose
  turn-1 events precede turn 2 renders **no negative offset**; each event's offset equals
  `TS - itsTurnStart`. This is the direct guard for the reported bug.
- **Grouping / collapse test**: a multi-turn root renders one header per turn; prior turns
  collapsed by default show header-only; the current turn is expanded; `enter` toggles a
  prior turn and the selection/scroll indices stay consistent.
- **Backward-compat test**: a single-turn root and a child node render byte-identically to
  the pre-change output (empty/one-element `Turns` ⇒ legacy path).
- **Blackboard test** (`blackboard_render_test.go` / `blackboard_scroll_test.go`): a
  two-turn blackboard shows a turn divider at the boundary, correct per-turn timing, and
  scroll offsets that account for the added header lines.

## Rollout / risk

- Pure TUI + in-memory node-model change; no wire, storage, or protocol impact, so no
  migration and nothing to feature-flag.
- Main risk is the selection/scroll bookkeeping in the detail and blackboard panes, which is
  already fiddly (`eventSel`, `eventScroll`, `blackboardGroupStarts`, wheel handlers). The
  backward-compat guard (`len(Turns) <= 1` ⇒ old path) contains the blast radius to the
  genuinely-multiturn case, and the regression tests lock the reported bug shut.

## Open questions (resolve at reconcile/build)

- Exact source of the turn index handed to `BeginTurn` vs `UserInputPayload.Turn` — confirm
  they are the same counter and pin it with a test.
- Whether blackboard entries carry a per-entry timestamp usable for bucketing, or turn
  attribution must be inferred from interleaving with the event stream.
- Collapsed-by-default vs remember-last-expanded for prior turns (ephemeral; pick the
  simpler at build time).
