<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0066 — Agents tab & blackboard — turn-aware multiturn UI (collapsible turn groups + per-turn timing)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0066-agents-tab-multiturn-turn-groups.md)**
<!-- docket:backlink:end -->

# Agents tab & blackboard — turn-aware multiturn UI — implementation plan

Change: **0066** · Spec:
`docs/superpowers/specs/2026-08-16-agents-tab-multiturn-turn-groups-design.md` (on the
`docket` branch) · Branch: `feat/agents-tab-multiturn-turn-groups` · Base: `origin/main`

> **Plan-role degradation.** `superpowers:writing-plans` is not available in this harness, so
> the `plan` role degraded to `auto` per the docket convention's missing-skill rule and this
> plan was authored directly by `docket-implement-next`. Artifact and stop-point are
> unchanged.

## Goal

Make the root `AgentNode` **turn-aware** so the agents tab and blackboard render a multi-turn
conversation correctly. Two operator-visible defects must be dead at the end:

1. **Negative event offsets** — `[-24013.7s]` on every prior-turn event, because
   `BeginTurn()` clobbers `root.StartedAt` while event offsets are computed against it.
2. **No turn separation** — the root detail transcript is one flat, unbounded list.

## Reconciled ground truth (read before starting)

These were corrected at reconcile; the spec's older wording elsewhere is superseded by them.

- `BeginTurn()` / `EndTurn(failed bool)` are methods on **`*AgentTree`**
  (`internal/agent/tree.go:330`, `:359`), each resolving the root via `t.Node(t.rootID)` and
  mutating under `root.mu`. `BeginTurn()` takes **no** argument and its signature must not
  change — the callers (`internal/tui/shell_model.go:1076`, `cmd/fuse/research_probe.go:158`)
  stay untouched.
- **Do NOT join on `evt.Turn`.** `loop.go`'s `turn` (`internal/agent/loop.go:402`) is the
  agent's inner tool-loop iteration counter; one conversational turn spans many of them.
  `TurnMark.Turn` is a **UI-local conversational ordinal**. Event→turn attribution is
  **timestamp bucketing only**.
- `NodeView` deliberately excludes `Events` (consumers call `CopyEvents()`), so `Turns` must
  be added to `NodeView` explicitly, as a **defensive copy** taken under the node lock.
- The blackboard has **no standalone renderer files** — `internal/tui/blackboard_*.go` are
  all tests. The render path is `internal/tui/agents_model.go`: `blackboardGroupStarts:437`,
  `blackboardContentWidth:455`, `buildBlackboardLines:880`, `blackboardBody:953`.
- `agent.BlackboardEntry.WrittenAt` (`internal/agent/blackboard.go:21`) exists — use it.

## Backward-compatibility guard (applies to every task)

Every new rendering path is gated on the root having **more than one** `TurnMark`:

```
len(n.Turns) > 1   ⇒ turn-aware path
otherwise          ⇒ today's code path, byte-identical
```

Child nodes never call `BeginTurn`, so their `Turns` is empty and they always take the old
path. A single-turn root also takes the old path. This is the blast-radius container — do not
weaken it.

## Relevant learnings (read at the named tasks)

- `border-inside-fixed-width-manual-join` — the detail pane owns an **exact-width invariant**;
  every row goes through `fitLine(..., w)`. New header/divider rows are no exception.
- `sanitize-untrusted-bytes-fixed-width-tui` — prompt previews and blackboard values are
  model/tool-controlled bytes: `sanitizeDisplay` + width-fit them, always.
- `teatest-final-frame-via-finalmodel-view` — for any screenshot capture, render
  `FinalModel().View()` (not `FinalOutput`) and force `termenv.TrueColor`.
- `verify-from-feature-worktree-binary` — Task 6 must build and run the binary **from this
  worktree**, never from the primary checkout.

---

## Task 1 — Node model: per-turn marks

**Files:** `internal/agent/tree.go`, `internal/agent/tree_test.go`

Write the tests first, then the implementation.

1. Add the type, exported, next to `AgentNode`:

   ```go
   // TurnMark records one conversational turn on the root node. Turn is a
   // UI-local 1-based ordinal — it is deliberately NOT event.UserInputPayload.Turn,
   // which counts the agent's inner tool-loop iterations (see change 0066).
   type TurnMark struct {
       Turn      int
       StartedAt time.Time
       EndedAt   time.Time // zero while the turn is in flight
   }
   ```

2. `AgentNode` gains `Turns []TurnMark` (guarded by the existing `n.mu`).

3. `NodeView` gains `Turns []TurnMark`. `Snapshot()` sets it to a **defensive copy**
   (`append([]TurnMark(nil), n.Turns...)`) so a UI consumer cannot mutate node state and the
   slice header does not race a concurrent append.

4. `BeginTurn()`:
   - appends `TurnMark{Turn: len(root.Turns) + 1, StartedAt: now}`;
   - sets `root.StartedAt = now` **only when it is currently zero** (preserving the
     documented "stays zero until the first BeginTurn" contract at `tree.go:273`, and never
     rewriting it afterward — this is what kills the negative offsets at the source);
   - keeps `Status = StatusRunning`, `EndedAt = time.Time{}`, and the existing `Emit`.

5. `EndTurn(failed bool)`: additionally stamps `EndedAt = now` on the **last** `TurnMark` if
   one exists and its `EndedAt` is zero. Earlier marks are never touched. Existing status /
   `root.EndedAt` / `Emit` behavior is unchanged.

**Tests (`tree_test.go`):**
- `BeginTurn` twice ⇒ `len(Turns) == 2`, ordinals `1, 2`, strictly increasing `StartedAt`.
- `StartedAt` is set by the first `BeginTurn` (from zero) and **unchanged** by the second.
- `EndTurn` stamps only the last mark; a prior mark's `EndedAt` is untouched.
- `Snapshot().Turns` is a defensive copy — mutating the returned slice's elements does not
  change a subsequent snapshot.
- A node that never sees `BeginTurn` has empty `Turns`.
- Concurrency: `BeginTurn`/`EndTurn` racing `Snapshot()` is clean under `-race`.

---

## Task 2 — Turn attribution + kill the negative offsets

**Files:** `internal/tui/agents_model.go`, new `internal/tui/turns_test.go`

**This task closes the reported bug.** Depends on Task 1.

1. Add the attribution helper (unexported, in `agents_model.go` near the detail helpers):

   ```go
   // turnStartFor resolves the start of the turn an event belongs to. Attribution
   // is purely by timestamp: Turns is append-ordered, so the answer is the latest
   // mark that had started by ts. It must NOT consult any event turn field
   // (see change 0066 — that index counts tool-loop iterations, not turns).
   func turnStartFor(n agent.NodeView, ts time.Time) time.Time
   ```

   - `len(n.Turns) == 0` ⇒ return `n.StartedAt` (legacy / child path).
   - Reverse-scan `n.Turns` for the last mark with `StartedAt <= ts`; return its `StartedAt`.
   - No mark qualifies (event predates turn 1) ⇒ return `n.Turns[0].StartedAt` so a
     pre-first-prompt event still renders a non-negative offset relative to the session's
     first turn.

2. Add a formatting helper so both call sites share one implementation, and **clamp at
   zero** — the defect is a negative number and the renderer must be structurally unable to
   emit one:

   ```go
   func eventOffset(n agent.NodeView, ts time.Time) string  // "%05.1fs", never negative
   ```

3. Replace both offset computations:
   - `buildEventViewLines` — `agents_model.go:1120`
   - `renderEventLines` — `agents_model.go:1240`

   Each becomes `ts = eventOffset(n, evt.TS)`, keeping the existing
   `if !n.StartedAt.IsZero()` guard and the `" 0.0s"` default.

**Tests (`turns_test.go`) — the direct regression guard:**
- **The reported bug:** a two-turn root whose turn-1 events precede turn 2's `StartedAt`.
  Assert **no rendered offset string starts with `-`** across the whole transcript. Assert
  each event's offset equals `TS - itsOwnTurnStart` to 0.1s.
- Turn-1 events attribute to turn 1 and turn-2 events to turn 2 even though the events carry
  no conversational turn index.
- An event exactly on a turn boundary (`TS == Turns[1].StartedAt`) attributes to turn 2.
- Legacy: empty `Turns` ⇒ offsets identical to `evt.TS.Sub(n.StartedAt)` (old behavior).
- An event predating turn 1 renders `0.0s`, not a negative.

---

## Task 3 — Headline timer reads the current turn

**Files:** `internal/tui/agents_model.go` (`nodeElapsed:1409`), `internal/tui/turns_test.go`

`nodeElapsed(n)` gains a turn-aware branch **before** its existing logic:

- `len(n.Turns) > 0` ⇒ use the **last** mark: `time.Since(last.StartedAt)` while
  `last.EndedAt` is zero, else `last.EndedAt.Sub(last.StartedAt)`.
- Otherwise unchanged (`n.StartedAt` zero ⇒ `"–"`; else `EndedAt-StartedAt` / `since`).

This preserves the original `BeginTurn` intent ("measures the turn, not the session") that
the Task-1 `StartedAt` fix would otherwise have silently changed — do not skip it.

**Tests:** a finished 2-turn root reports turn 2's duration (not the whole session's); an
in-flight turn counts from the current mark; empty `Turns` is unchanged; a zero `StartedAt`
still renders `"–"`.

---

## Task 4 — Collapsible per-turn groups in the detail pane

**Files:** `internal/tui/agents_model.go`, `internal/tui/turn_groups_test.go`

The fiddliest task — the spec names selection/scroll bookkeeping as the main risk. Depends
on Tasks 1–2.

### 4a. Row model

Today `renderEventLines` emits exactly one line per event, so `m.eventSel` (event index) and
the rendered-line index coincide; inserting headers breaks that 1:1. Introduce an explicit
row model as the single source of truth:

```go
type detailRow struct {
    header bool // true = turn header row
    turn   int  // conversational ordinal this row belongs to
    evtIdx int  // index into `visible`; -1 for a header row
}
```

`func (m *AgentsModel) detailRows(n agent.NodeView, visible []agent.AgentEvent) []detailRow`

- **Guard:** `len(n.Turns) <= 1` ⇒ return one non-header row per event, in order. Everything
  downstream then behaves exactly as today.
- Otherwise: bucket events by `turnStartFor` (Task 2 — the *same* helper, no second rule).
  Emit a header row per turn, then that turn's event rows **unless** the turn is collapsed.
  Events predating turn 1 form a leading bucket rendered under turn 1's header.
- **Default collapse:** every turn except the last is collapsed. The last (current) turn is
  expanded.

### 4b. View state

`AgentsModel` gains `turnExpanded map[int]bool` — ephemeral, never persisted. Semantics:
absent ⇒ the default above; present ⇒ the operator's explicit choice. Add `rowSel int`, the
index into `detailRows`.

Keep `m.eventSel` as an index into `visible` — `buildEventViewLines` reads
`visible[m.eventSel]` and must not change meaning. Whenever `rowSel` lands on an event row,
set `m.eventSel = row.evtIdx`; the two stay in lockstep.

### 4c. Header row rendering

`turn N · "<prompt preview>" · <duration>`

- Prompt preview: the turn's first `KindUserInput` event content, `sanitizeDisplay`d and
  truncated; `"(no prompt)"` when absent.
- Duration: `EndedAt-StartedAt`, or `running` while `EndedAt` is zero.
- A collapsed header additionally shows its event count (e.g. `▸ 24 events`); expanded shows
  `▾`.
- Rendered through `fitLine(..., w)` — the **exact-width invariant** is not negotiable
  (learning `border-inside-fixed-width-manual-join`).

### 4d. Navigation

- `renderEventLines` renders from `detailRows`, one line per row, highlighting `rowSel`.
- `handleDetailKey` `j`/`k` move `rowSel` over rows (headers included) instead of `eventSel`
  over events; `g`/`G` jump to first/last row. `followTail` continues to pin to the **last**
  row.
- `enter` (`handleDetailKey`): on a **header** row, toggle `turnExpanded[row.turn]` and
  return (do **not** enter the event view); on an **event** row, today's behavior exactly —
  set `m.eventSel` and enter the event view.
- `buildDetailLines`' scroll clamp already operates on rendered lines (`len(all) - rows`), so
  it keeps working; the "keep selection visible" window math must key off `rowSel`, not
  `eventSel`.
- `m.eventCount` continues to count **events** (not rows) — other code reads it.

**Tests (`turn_groups_test.go`):**
- A 3-turn root renders exactly 3 header rows, in turn order.
- Default state: turns 1–2 collapsed (header only, no event rows), turn 3 expanded with all
  its event rows present.
- `enter` on a collapsed prior-turn header expands it; the row count grows by exactly that
  turn's event count; `enter` again restores it.
- After a toggle, `rowSel` still points at the same header and `eventSel` is within
  `[0, len(visible))`.
- `enter` on an event row still opens the event view showing **that** event.
- **Width invariant:** every rendered detail row is exactly `w` cells, across widths 80/100/120
  and both collapsed and expanded states.
- **Backward-compat:** a single-turn root and a child node render **byte-identically** to the
  pre-change output (capture the old output in the test from the legacy path).
- A prompt preview containing ESC/CR/tab bytes is sanitized and cannot overflow the row.

---

## Task 5 — Blackboard turn awareness

**Files:** `internal/tui/agents_model.go` (`blackboardBody:953`), `internal/tui/blackboard_render_test.go`,
`internal/tui/blackboard_scroll_test.go`

Depends on Tasks 1–2.

**Preserve the writer-group structure.** Groups are keyed by writer and ordered
most-recent-first, and `blackboardGroupStarts` (`:437`) records **writer-group** start lines
for `n`/`p` navigation — that contract stays. Turn awareness goes *inside* a group:

- `blackboardBody` needs the root `NodeView` to reach `Turns`; pass it in (or read it off
  `m.nodeByID[m.tree.RootID()]`) — keep the accessor identical between `blackboardBody` and
  `blackboardGroupStarts` so their line counts cannot diverge (that equality is exactly what
  `blackboardContentWidth`'s comment already protects).
- Within each writer group, bucket the group's keys by the turn that
  `turnStartFor(root, e.WrittenAt)` resolves — the **same** helper as the event path — then
  sort alphabetically within each bucket (today's ordering, now per-bucket). Order buckets by
  turn ascending.
- When a group spans more than one turn, emit a **turn sub-divider** row before each bucket:
  `── turn N ──`, `fitLine`d to `w`. A single-turn group (or `len(Turns) <= 1`) emits **no**
  divider — byte-identical to today.
- Per-entry meta line gains the turn-relative offset (`+12.3s`), computed with the Task-2
  clamped helper so it can never be negative.
- `groupStarts` continues to append `len(body)` at each **writer**-group start, so the extra
  divider lines are absorbed automatically and `n`/`p` stays exact.

**Tests:**
- A blackboard whose entries span two turns shows a divider at the boundary, with entries on
  the correct side of it.
- Per-entry offsets are non-negative and are relative to the entry's own turn.
- `blackboardGroupStarts()` still returns the writer-group start lines and matches the render
  path's line numbering exactly with dividers present (this is the scroll-correctness guard).
- Single-turn / no-turn boards render byte-identically to today.
- The existing blackboard render, scroll, tab, and e2e tests still pass unchanged.

---

## Task 6 — Live verification: drive the real application

**Files:** `docs/results/2026-08-16-agents-tab-multiturn-turn-groups-results.md`

The two defects are **operator-visible**, and the automated suite renders through model code
rather than a real session — so a green suite is necessary but not sufficient. This task
exercises the UI and its telemetry by driving the actual application.

**Build from this worktree** (learning `verify-from-feature-worktree-binary` — a binary built
from the primary checkout will not contain this branch):

```
cd .worktrees/agents-tab-multiturn-turn-groups && go build -o ./fuse ./cmd/fuse
```

Drive a **genuine multi-turn session** (a scripted `LLM_GATEWAY_URL` double is fine and
preferred over paid traffic — see the repo's existing e2e gateway doubles; per house policy
live verification traffic never uses Anthropic models). Send at least **three** prompts on one
`loop_id`, with tool calls in at least two of them so each turn carries a real event burst.

Observe and record, with captured frames where practical (use
`FinalModel().View()` + forced `termenv.TrueColor`, learning
`teatest-final-frame-via-finalmodel-view`):

1. **No negative offsets** anywhere in the root detail pane, on any turn, after turns 2 and 3
   begin — the exact symptom from the bug report. Scroll the full transcript.
2. **Turn group headers** render with the right ordinal, a readable prompt preview, and a
   plausible duration; prior turns are collapsed and the current one expanded.
3. **`enter` toggles** a prior turn's group; selection and scroll stay coherent across
   several toggles and after new events arrive mid-session.
4. **Headline timer** tracks the *current* turn, resetting at each new prompt rather than
   counting the whole session.
5. **Blackboard** (`b`) shows turn separation, non-negative per-entry offsets, and `n`/`p`
   still lands exactly on writer-group headers with dividers present.
6. **Telemetry** — confirm the change did not disturb the event/observability path: the
   emitted `turn.start` / `turn.end` events and the OTEL/metrics wiring behave as before
   across all three turns (this change consumes the stream and must not alter it).

Record each observation — pass/fail, what was seen — in the results file, and note anything a
human should re-check at the merge gate. **A failure here is a defect in this branch**: fix it
and re-verify, do not write it up as a caveat.

---

## Sequencing

Task 1 → Task 2 → (Tasks 3, 4, 5 in parallel-safe order; 4 is the largest) → Task 6.
Tasks 3, 4 and 5 all depend on Task 2's `turnStartFor` — do not fork a second attribution
rule anywhere.

## Suite gate

`go build ./... && go test ./... -race` green at the end, plus `gofmt`. Task 6's live
verification runs after the suite is green.

## Out of scope (from the spec — do not drift)

No event-stream wire-format, durable-stream, or `reconstruct.go` change. No child-agent,
segment-view, or scheduler change. No cross-session persistence of collapse state. No new
keybindings beyond reusing `enter`.
