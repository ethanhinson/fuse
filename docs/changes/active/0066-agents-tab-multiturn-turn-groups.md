---
id: 66
slug: agents-tab-multiturn-turn-groups
title: Agents tab & blackboard — turn-aware multiturn UI (collapsible turn groups + per-turn timing)
status: in-progress
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
branch: feat/agents-tab-multiturn-turn-groups
pr:
claimed_at: 2026-08-16T21:17:40Z
blocked_by:
reconciled: true
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

All resolved at reconcile (2026-08-16) — see the spec's *Open questions* section:

- ~~Is `BeginTurn`'s turn index the same counter as `UserInputPayload.Turn`?~~ **No.** They
  are different counters; the join is unsound and has been removed from the design.
- ~~Do blackboard entries carry a per-entry timestamp usable for turn bucketing?~~ **Yes** —
  `BlackboardEntry.WrittenAt`.

## Verification

Beyond the automated suite, both defects are operator-visible, so this change is verified by
**driving the real application** through a genuine multi-turn shell session and observing the
UI and its telemetry directly — offsets never negative across a turn boundary, turn-group
headers correct, `enter` collapse/expand stable, headline timer scoped to the current turn,
blackboard turn separation and timing correct. Observations are recorded in the results file.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-16

Reconciled against `origin/main` at claim time. The change remains valid and in scope — both
defects still reproduce in current code, and none of the work has been done elsewhere. Four
corrections were folded into the spec:

1. **The UI join key was wrong — design corrected.** The spec assumed `TurnMark.Turn` could
   be joined to `event.UserInputPayload.Turn`. It cannot: `loop.go`'s `turn`
   (`internal/agent/loop.go:402`) is the agent's **inner tool-loop iteration** counter — one
   conversational turn spans many of them — while `BeginTurn`/`EndTurn` are **conversational**
   boundaries driven from the TUI shell (`internal/tui/shell_model.go:1076`, `:681`). The
   exact-join is removed; event→turn attribution is now **solely** timestamp bucketing
   against the append-ordered `Turns` marks, and the "pin the equality with a test"
   obligation is replaced by its inverse.
2. **Receivers corrected.** `BeginTurn`/`EndTurn` are methods on **`*AgentTree`**
   (`internal/agent/tree.go:330`, `:359`), not `AgentNode`, and `BeginTurn()` takes **no**
   argument. Its signature stays unchanged so neither caller (`shell_model.go:1076`,
   `cmd/fuse/research_probe.go:158`) needs touching; the ordinal is assigned internally.
3. **`NodeView` snapshot note.** `NodeView` deliberately excludes `Events` (consumers use
   `CopyEvents()`), so `Turns` must be added to `NodeView` explicitly, as a defensive copy
   taken under the node lock.
4. **Blackboard file layout corrected.** There are no standalone `blackboard_*.go`
   renderers — those files are all tests. The render path lives entirely inside
   `internal/tui/agents_model.go` (`blackboardGroupStarts:437`, `buildBlackboardLines:880`,
   `blackboardBody:953`). `BlackboardEntry.WrittenAt` (`internal/agent/blackboard.go:21`)
   confirmed present, so the same bucketing rule applies there.

Also added an explicit **verification obligation**: drive the real app through a multi-turn
session and observe the UI + telemetry, recording it in the results file.

No adjacent follow-up work met the materiality bar. (`AUTO_CAPTURE_ENABLED` is `false` for
this repo, so nothing would have been minted regardless.)
