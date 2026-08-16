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

- `AgentNode.Turns []TurnMark`, snapshotted into `NodeView` as a **defensive copy** taken
  under the node lock. (`NodeView` deliberately excludes `Events` — those go through
  `CopyEvents()` — so `Turns` must be added to `NodeView` explicitly; it is small and
  bounded by the conversation's turn count, so copying it per snapshot is fine.)
- **Receivers (reconciled 2026-08-16):** `BeginTurn` and `EndTurn` are methods on
  **`*AgentTree`**, not `AgentNode` — `func (t *AgentTree) BeginTurn()` (`tree.go:330`) and
  `func (t *AgentTree) EndTurn(failed bool)` (`tree.go:359`). Each resolves the root via
  `t.Node(t.rootID)` and mutates it under `root.mu`. The `Turns` slice lives on the root
  `AgentNode`; the tree methods append/stamp it.
- `BeginTurn()` **appends** a new `TurnMark{Turn: n, StartedAt: now}` with `EndedAt` zero,
  instead of overwriting `StartedAt`. It still sets `Status = StatusRunning`, clears
  `EndedAt`, and emits. The root's overall `StartedAt` is set **once** on the first
  `BeginTurn` (it stays zero before then — `tree.go:273`) and is never rewritten, so
  anything reading `StartedAt` as "when did this node begin" stays correct.
- `EndTurn(failed)` stamps the **current** (last) `TurnMark.EndedAt` and freezes status.
- **`BeginTurn()` keeps its no-argument signature.** Its two callers
  (`internal/tui/shell_model.go:1076`, `cmd/fuse/research_probe.go:158`) are left untouched;
  the mark's ordinal is assigned internally as `len(Turns)+1` (1-based, display-facing).

#### Resolved: the turn ordinal is UI-local — do NOT join on `evt.Turn`

The spec originally assumed `TurnMark.Turn` could be joined to
`event.UserInputPayload.Turn`. **Reconcile establishes that they are different counters and
the join is unsound:**

- `loop.go`'s `turn` is the **agent's inner tool-loop iteration** index —
  `for turn := 0; a.maxTurns <= 0 || turn < a.maxTurns; turn++` (`loop.go:402`). It is what
  is stamped into `UserInputPayload{Turn: turn}` (`:420`), `TurnStartPayload` (`:408`) and
  `TurnEndPayload` (`:566`, `:630`, `:642`, `:652`, `:696`). A single conversational turn
  routinely spans **many** of these iterations.
- `BeginTurn`/`EndTurn` are **conversational** turn boundaries driven from the TUI shell
  (`shell_model.go:1076` / `:681`) — one call per human prompt.

So an `evt.Turn` of 7 says "agent tool-loop iteration 7", not "conversational turn 7", and
the two indices coincide only by accident. **`TurnMark.Turn` is therefore a UI-local
conversational ordinal, and event→turn attribution is done SOLELY by timestamp bucketing**
(next section). The `evt.Turn` exact-join described below is **removed from the design**, and
the corresponding "pin the equality with a test" obligation is replaced by its inverse: a
test asserting attribution is timestamp-derived and does not read `evt.Turn`.

Only the **root** node accumulates turns. Child nodes never call `BeginTurn`, so their
`Turns` slice stays empty and every consumer treats "empty Turns" as the single-turn legacy
case — this is the backward-compat and child-node path in one predicate.

### Per-event offset resolution

A small helper resolves an event's turn start:

```
turnStartFor(n NodeView, evt) time.Time:
    if len(n.Turns) == 0 { return n.StartedAt }          // legacy / child: unchanged
    pick the LATEST TurnMark whose StartedAt <= evt.TS   // sole attribution rule
    if none (event predates the first turn): return n.StartedAt
```

Attribution is **purely by timestamp bucketing** — `Turns` is append-ordered and therefore
already sorted by `StartedAt`, so this is a reverse scan (or binary search) with no
dependence on any wire field. This is deliberately robust: it needs no protocol change, it
works for every event kind (most carry no conversational turn index at all), and per the
resolved decision above it must **not** consult `evt.Turn`.

The detail renderer (`renderEventLines`, `agents_model.go:1235-1277`, offset at `:1240`) and
the expanded-event title (`buildEventViewLines`, offset at `:1120`) use
`evt.TS.Sub(turnStartFor(n, evt))` instead of `evt.TS.Sub(n.StartedAt)`. Offsets become
correct and non-negative for every turn.

Clamp the result at zero as a belt-and-braces guard: the reported defect is a *negative*
offset, so the regression test asserts non-negativity and the renderer must not be able to
violate it even if a clock skews.

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

**Reconciled:** there are no standalone `blackboard_*.go` renderer files — the
`blackboard_*` files under `internal/tui` are all **tests**. The blackboard render path
lives entirely in `internal/tui/agents_model.go`: `blackboardGroupStarts` (`:437`),
`blackboardContentWidth` (`:455`), `buildBlackboardLines` (`:880`), `blackboardBody`
(`:953`). Edit those.

**Resolved open question:** blackboard entries **do** carry a usable per-entry timestamp —
`agent.BlackboardEntry.WrittenAt time.Time` (`internal/agent/blackboard.go:21`, stamped by
`Put()` at `:61`, and already used for group ordering at `:968`/`:979-982`). So turn
attribution uses the **same timestamp bucketing** as the event path; no inference from
event-stream interleaving is needed.

Apply the analogous treatment:

- Attribute each blackboard entry to a turn by bucketing `WrittenAt` against the root's
  `Turns` marks — the identical `turnStartFor` rule.
- Insert a **turn divider / header** at each turn boundary in the blackboard body, and make
  any per-entry relative timing use the entry's turn start (fixing the same negative-offset
  class if present). Collapse of prior turns is desirable for consistency but is the lower
  bar — at minimum the blackboard must show correct timings and clear turn separation.
- `blackboardGroupStarts` (`:434`) and the blackboard scroll model already track
  body-line group offsets; extend them to include turn headers so scroll/selection stays
  correct.

## Testing strategy

- **Node model unit tests** (`internal/agent/tree_test.go` — exists): `BeginTurn` appends a
  `TurnMark` and does **not** rewrite an already-set `StartedAt` (while still setting it on
  the very first call, from zero); `EndTurn` stamps the current mark's `EndedAt` and leaves
  earlier marks untouched; `NodeView.Turns` is a defensive copy (mutating it does not affect
  the node); a child node that never sees `BeginTurn` has an empty `Turns`.
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

## Open questions — all resolved at reconcile (2026-08-16)

- ~~Exact source of the turn index handed to `BeginTurn` vs `UserInputPayload.Turn`.~~
  **RESOLVED: they are different counters and must not be joined.** `BeginTurn()` takes no
  index; `loop.go`'s `turn` is the inner tool-loop iteration. `TurnMark.Turn` is a UI-local
  conversational ordinal (`len(Turns)+1`) and attribution is timestamp bucketing only.
- ~~Whether blackboard entries carry a per-entry timestamp usable for bucketing.~~
  **RESOLVED: yes** — `BlackboardEntry.WrittenAt` (`internal/agent/blackboard.go:21`).
- ~~Collapsed-by-default vs remember-last-expanded.~~ **RESOLVED: collapsed-by-default**
  (the simpler option, per the spec's own tie-break), with `enter` toggling; state is an
  ephemeral map on `AgentsModel`.

## Verification obligation (added at reconcile)

This change fixes two **operator-visible** defects, so automated tests alone are not the
whole receipt. Before the PR opens, the UI and its telemetry must be exercised by driving
the real application: a genuine multi-turn session in the shell, confirming (a) no negative
`[Ns]` offsets anywhere in the root detail pane across turn boundaries, (b) per-turn group
headers render with turn number, prompt preview and duration, (c) `enter` collapses and
expands a prior turn without corrupting selection or scroll, (d) the headline timer tracks
the current turn, and (e) the blackboard shows turn separation with correct per-entry
timing. Record the observations in the change's results file.
