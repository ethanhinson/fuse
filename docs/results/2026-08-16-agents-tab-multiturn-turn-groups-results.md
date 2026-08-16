<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0066 — Agents tab & blackboard — turn-aware multiturn UI (collapsible turn groups + per-turn timing)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0066-agents-tab-multiturn-turn-groups.md)**
<!-- docket:backlink:end -->

# Agents tab & blackboard — turn-aware multiturn UI — results

Change [#0066](../changes/active/0066-agents-tab-multiturn-turn-groups.md) ·
Branch `feat/agents-tab-multiturn-turn-groups` · Base `origin/main` @ `4089604`

## Outcome

Both operator-visible defects are dead, confirmed by driving the real application rather
than by reading model code. A genuine three-prompt session on one `AgentTree` — real
keystrokes, real `agent.Agent` loop, real `model.Adapter`, real tool executor, real
`/agents` overlay — renders **no negative offset anywhere**, groups the transcript into
collapsible per-turn groups, and separates the blackboard by turn. One defect was found
and fixed during this verification (zero-padded blackboard offsets, below).

## How it was driven

Live harness: `internal/tui/multiturn_live_e2e_test.go`
(`TestLiveMultiTurn_TurnGroupsAndOffsets`), built on the repo's established teatest +
`FUSE_SCREENSHOT_DIR` harness (`internal/tui/harness_test.go`).

- **Scripted gateway double**, not a fake completer: an `httptest` server speaking the
  OpenAI-compatible wire, with a real `model.NewAdapter` in front of it. The double
  answers from the request state it is sent (user-message count = conversational turn;
  a trailing `tool` message closes the turn), so it cannot drift out of step with the loop.
  Per house policy no Anthropic/Claude model is involved — and in fact no network egress
  at all, the gateway is on `127.0.0.1`.
- **Three prompts on one session**, tool calls in the first two:
  1. `record the first plan step on the board` → `blackboard_write plan/turn-one`
  2. `now record the second plan step` → `blackboard_write plan/turn-two`
  3. `summarize what you recorded` → no tool call
  Each gateway reply is delayed 400ms so every turn occupies real wall-clock time and the
  offsets are non-trivial.
- Both tool calls were asserted to have reached the **real** `agent.Blackboard` store, and
  the root node was asserted to carry exactly **3 `TurnMark`s** and a real event burst,
  before any UI assertion ran.

**Worktree binary.** `go build -o ./fuse ./cmd/fuse` from
`.worktrees/agents-tab-multiturn-turn-groups` (learning `verify-from-feature-worktree-binary`).
The binary carries this branch — `strings ./fuse` contains `internal/tui.turnPromptPreview`
and the `turn %d` header format, neither of which exists on `origin/main` — and completes a
one-shot turn against the same scripted gateway (`./fuse -model glm -approve-all "say hello"`
→ `binary-live-ok`).

## Observations

### 1. No negative offsets — PASS

The exact reported symptom (`[-24013.7s]`) does not occur. All three turn groups were
expanded and the transcript scrolled top to bottom; every offset ever painted was matched
against `-\d+\.\d+s` and there were zero hits, on the detail pane and on the blackboard.
The full expanded transcript, scrolled to the top:

```
│▾ turn 1 · "record the first plan step on the board" · 1s             │
│[000.4s] ▸ tool_call  blackboard_write({"key":"plan/turn-one","value":│
│[000.4s] ◂ result     wrote key "plan/turn-one"                       │
│[000.8s] ▸ assistant  turn-one-complete                               │
│▾ turn 2 · "now record the second plan step" · 1s                     │
│[000.4s] ▸ tool_call  blackboard_write({"key":"plan/turn-two","value":│
│[000.4s] ◂ result     wrote key "plan/turn-two"                       │
│[000.8s] ▸ assistant  turn-two-complete                               │
│▾ turn 3 · "summarize what you recorded" · 0s                         │
│[000.4s] ▸ assistant  turn-three-complete                             │
```

Note that turn 2's and turn 3's events restart from `000.4s` rather than continuing to
climb — that is the fix: each event is measured against **its own** turn's start.

### 2. Turn group headers — PASS

Correct ordinals, readable prompt previews, plausible durations, prior turns collapsed and
the current turn expanded, all at the default state on first opening the pane:

```
│▸ turn 1 · "record the first plan step on the board" · 1s · 3 events  │
│▸ turn 2 · "now record the second plan step" · 1s · 3 events          │
│▾ turn 3 · "summarize what you recorded" · 0s                         │
│[000.4s] ▸ assistant  turn-three-complete                             │
```

The `3 events` suffix on a collapsed header is accurate against the node's real events, and
turn 3 (`0s`) is genuinely shorter than turns 1 and 2 (`1s`) — it made no tool call.

### 3. `enter` toggles a prior turn's group — PASS

`g` to turn 1's header, then three `enter`s (expand / collapse / expand), each verified
against a **forced full repaint** so the assertion reads current state rather than a stale
frame. After the third toggle the group is expanded, the cursor is still on turn 1's header,
and turn 1's real events are present:

```
│▾ turn 1 · "record the first plan step on the board" · 1s             │
│[000.4s] ▸ tool_call  blackboard_write({"key":"plan/turn-one","value":│
│[000.4s] ◂ result     wrote key "plan/turn-one"                       │
│[000.8s] ▸ assistant  turn-one-complete                               │
│▸ turn 2 · "now record the second plan step" · 1s · 3 events          │
│▾ turn 3 · "summarize what you recorded" · 0s                         │
│[000.4s] ▸ assistant  turn-three-complete                             │
```

Turn 2 was then expanded from four rows further down; selection and scroll stayed coherent
across `G`, `g` and 30 `j` presses with the row count changing underneath.

> **Harness note, not a product defect.** During this work the toggle *appeared* broken.
> It was not: bubbletea's renderer emits only changed lines, so the most recent complete
> frame in the output stream can be many keystrokes stale. Reading UI state off it lies.
> `forceRepaint` (a window-size wiggle) is now in the harness with that explanation, and any
> future TUI verification that asserts *current* state must go through `waitForFrame`, not
> `waitFor`.

### 4. Headline timer tracks the current turn — PASS

The tree row and detail header both read `alpha ... ✓ 0s` at the end of a session whose
wall-clock length was ~2.4s: the timer is showing turn 3's duration, not the session's.
Asserted numerically too — `nodeElapsed(root)` equals turn 3's `EndedAt - StartedAt`, and
turn 3's duration is under three quarters of the measured session elapsed.

### 5. Blackboard — PASS, after fixing one defect

Turn separation, non-negative per-entry offsets, and `n`/`p` group navigation all hold:

```
│▌ alpha                                                               │
│── turn 1 ──                                                          │
│plan/turn-one                                                         │
│  ⟨written by alpha · +0.4s⟩                                          │
│  {                                                                   │
│    "step": 1                                                         │
│  }                                                                   │
│── turn 2 ──                                                          │
│plan/turn-two                                                         │
│  ⟨written by alpha · +0.4s⟩                                          │
│  {                                                                   │
│    "step": 2                                                         │
│  }                                                                   │
```

`blackboardGroupStarts()` was checked against the render path's own line numbering with
dividers present, and every reported start line is a writer header (`▌ `) — so `n`/`p`
still lands exactly on writer-group headers and the sub-dividers are absorbed.

**Defect found and fixed.** The per-entry offset originally rendered zero-padded as
`⟨written by alpha · +000.4s⟩`, because it reused the detail pane's `%05.1fs`. Judgement:
that is a defect, not a cosmetic preference. The detail pane pads because the offset sits
inside a `[…]` column that must stay aligned; the blackboard meta is inline prose, where
`+000.4s` reads as a rendering fault rather than a duration — an operator would file it.
Fixed by adding `turnOffsetLabel` (same `turnStartFor` attribution, same zero clamp, no
padding) and using it only for the board. `TestBlackboardEntryOffsetsAreTurnRelative` now
pins the unpadded form and rejects any zero-padded offset on the board, and the live test
asserts the same against the running program.

### 6. Telemetry undisturbed — PASS

This change consumes the event stream; it must not alter it.

- **By diff:** the branch touches no file under `internal/event/` or `internal/observe/`.
  `internal/agent/loop.go` — the only emitter of `turn.start`/`turn.end` — is untouched.
- **By live observation:** a real `event.EventStore` was attached to every turn's agent in
  the session above. The session emitted **5 `turn.start` and 5 `turn.end`**, and the
  `turn.start` `Turn` sequence was exactly `[0 1 0 1 0]` — the loop's 0-based *inner*
  iteration counter, restarting at each prompt. The UI's conversational ordinal (which
  would have produced a monotonic sequence) has not leaked into the stream. This is the
  distinction the plan's reconcile called out, now confirmed at runtime.
- **By suite:** `go test ./cmd/fuse/ -run 'Observ|TurnStart|TwoBindings|Wiring'` green,
  covering `observe_wiring_test.go`, `observability_acceptance_test.go` and
  `two_bindings_parity_test.go`.

## Test results

- `go build ./...` green.
- `go test ./internal/tui/ ./internal/agent/ -race -count=1` green.
- `go test ./cmd/fuse/ -run 'Observ|TurnStart|TwoBindings|Wiring' -count=1` green.
- New durable e2e: `internal/tui/multiturn_live_e2e_test.go`.

## For the human at the merge gate

- **`freeze` is not installed here**, so screenshot captures produced `.ansi`/`.txt` only,
  no PNG. The frames quoted above are the real captures. Install
  `github.com/charmbracelet/freeze` and re-run with `FUSE_SCREENSHOT_DIR` set if a PNG is
  wanted for the PR.
- **Worth eyeballing in a real terminal:** the reverse-video cursor on a turn *header* row.
  Text captures strip ANSI, so cursor styling on headers versus event rows was verified
  structurally (row model + width invariant), not visually.
- **`gofmt` reports `internal/tui/shell_model.go` unformatted** — this is pre-existing on
  `origin/main` (verified by piping `origin/main`'s copy through `gofmt -l`), not introduced
  by this branch, and was deliberately left alone rather than swept into this change's
  commit.
- The live test's turn timings depend on a 400ms scripted gateway pause; it is not
  wall-clock sensitive beyond "turn 3 is shorter than the session", but it is the one
  assertion that could get flaky on a very loaded machine.

## Whole-branch review and in-branch fixes (2026-08-16, post-verification)

The branch was reviewed at the **deep** rung (selected by rule: the highest build profile any task
routed to was `premium`, and the whole-branch diff exceeded the 1500-line bump threshold). The
reviewer returned **8 findings: 0 blocker, 1 important, 7 minor**, and explicitly cleared the seven
risks it was asked to scrutinize — single-rule turn attribution, the `len(Turns) <= 1` backward-compat
guard, the exact-width invariant, selection/scroll bookkeeping, the `blackboardGroupStarts` line-number
equality, and the `Turns` locking discipline.

All 8 were fixed in-branch across three tasks, then the full suite was re-run green:

| # | Sev | Finding | Fixed by |
|---|---|---|---|
| 1 | important | Turn-header prompt preview did not reserve a cell for the ellipsis, so an overflowing prompt pushed the header to `w+1` cells and `fitLine` silently clipped the **duration / event-count suffix** (`… · running` → `… · runnin`) | `2519d86` |
| 3 | minor | Root cause of #1 — a cell-denominated budget was passed to a **byte**-denominated truncator, which also under-filled CJK/emoji previews by ~2/3 | `2519d86` |
| 2 | minor | The test meant to catch #1 passed for the wrong reason (its fixture's multibyte prefix masked the byte/cell mismatch) | `2519d86` |
| 5 | minor | Toggling the **current** turn was not symmetric: `followTail` stayed set, so expanding re-snapped the cursor onto an event and the next `enter` drilled in instead of collapsing back | `d7b495a` |
| 6 | minor | On a turn-aware board, a writer group confined to one turn printed turn-relative offsets with **no turn label** | `d7b495a` |
| 7 | minor | `renderEventLines` had become dead production code carrying a comment claiming it was still on the render path | `d7b495a` |
| 8 | minor | The live e2e was wall-clock-coupled (fixed sleeps, a duration-ratio assertion) and flake-prone under load | `d7b495a` |
| 4 | minor | `syncEventSel` took a `rows` parameter it never read, hiding its real dependency on `m.detRows` | `71a4d0e` |

Finding 1 is the one worth a second look at the merge gate: it was invisible to
`TestDetailRowsWidthInvariant` precisely because `fitLine` guarantees the width that test asserts —
the row was always `w` cells, it was the *content* that was being eaten. The fix truncates in display
cells via a new `truncateCells` helper and the regression test now asserts the header's suffix
survives verbatim rather than just checking the row width.

**Live verification still holds after the fixes.** `internal/tui/multiturn_live_e2e_test.go` runs as
part of `make test` (it self-skips only under `-short`), so the post-fix green suite re-drove the full
three-prompt session end to end. Finding 8's de-flaking also added a `-short` skip, so a maintainer
can now isolate the slow live test.

Build evidence at the final commit:

```
command:  make test
result:   green
head_sha: 71a4d0eec2f56a6f27d593487ef40599d7e16335
ran_at:   2026-08-16T22:24:19Z
```

## Process note for the maintainer (not a code finding)

This repo has **no `finalize.test_command` configured**, and docket's shipped suite auto-detection
looks for `tests/test_*.sh`, which matches nothing in a Go repo. The build gate therefore ran the
repo's own declared suite, `make test`. Setting `finalize.test_command: make test` in `.docket.yml`
would make the gate unambiguous for every future change and is worth doing before the next run.
