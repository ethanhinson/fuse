---
id: 66
slug: agents-tab-multiturn-turn-groups
title: Agents tab & blackboard — turn-aware multiturn UI (collapsible turn groups + per-turn timing)
status: proposed
priority: medium
type: fix
created: 2026-08-16
updated: 2026-08-16
depends_on: []
related: [12, 53, 54]
discovered_from: [53]
adrs: []
spec: docs/superpowers/specs/2026-08-16-agents-tab-multiturn-turn-groups-design.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-16-agents-tab-multiturn-turn-groups-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-16-agents-tab-multiturn-turn-groups-design.md) |
<!-- docket:artifacts:end -->

## Why

Change #53 made the shell's root loop a **persistent, multi-turn conversation** (one
`loop_id`, park-at-turn-end, `BeginTurn()` per new prompt). The agents tab and blackboard —
built for the single-turn spawn tree of #12 — never caught up, and two defects show up live
the moment a second turn starts:

- **The detail transcript doesn't separate turns.** A second turn's events append to the
  first with no header, divider, or collapse; the pane grows unbounded and the operator
  can't tell where one turn ends and the next begins.
- **Event timestamps go negative.** Each event's `[Ns]` offset is computed against
  `root.StartedAt`, but `BeginTurn()` clobbers `StartedAt = now` every turn — so all
  prior-turn events suddenly render huge negative offsets (`[-24013.7s]` observed). The
  timings are simply wrong for every event outside the current turn.

The blackboard view shares the same root loop and single-clock assumption, so it needs the
same turn-aware treatment for a consistent operator mental model.

## What changes

Make the root node **turn-aware** and have both views read that model:

- Node model: replace the single clobbered clock with per-turn marks (`TurnMark{Turn,
  StartedAt, EndedAt}`). `BeginTurn` appends a mark instead of overwriting `StartedAt`;
  `EndTurn` stamps the current mark. Child nodes (single-turn) are unchanged.
- Detail pane: resolve each event's offset against **its own turn's start** (kills the
  negative numbers), and render the root transcript as **collapsible per-turn groups** —
  prior turns collapse to a one-line header (turn #, prompt preview, duration), the current
  turn stays expanded, `enter` toggles a group.
- Root headline timer shows the **current turn's** elapsed (preserving today's `BeginTurn`
  intent).
- Blackboard: apply the analogous turn-aware treatment — group/divide writer entries by
  turn and fix per-turn timing.

Design detail, data-model shape, and the per-view fallback/bucketing rules live in the
linked spec.

## Out of scope

- No change to the event stream wire format, durable stream, or `reconstruct.go` — the
  turn index already exists; this consumes it.
- No change to child-agent (subagent) rendering, the segment view, or the scheduler.
- No cross-session persistence of collapse/expand state (ephemeral view state).
- No new keybindings beyond reusing `enter` for the turn-group toggle.

## Open questions

- Confirm the turn index handed to `BeginTurn` is the same counter as
  `UserInputPayload.Turn`, and pin the equality with a test (it's the UI join key).
- Whether blackboard entries carry a per-entry timestamp usable for turn bucketing, or turn
  attribution must be inferred from event-stream interleaving.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
