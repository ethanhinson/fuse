<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0039 — Agents tab — timer runs before the first prompt; shows the default model, not the selected one](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0039-agents-tab-idle-timer-and-model.md)**
<!-- docket:backlink:end -->

# Plan — Agents tab: idle timer before first prompt + live model label (change 0039)

> Plan role degraded to `auto`: `superpowers:writing-plans` is not installed on
> this machine, so this plan file was authored inline by docket-implement-next.

## Context

Two display bugs on the agents tab (shipped in change 0012):

1. **Elapsed timer runs before the first prompt.** `NewAgentTree()` stamps the
   root node with `StartedAt: time.Now()` at construction
   (`internal/agent/tree.go:240`), so opening the agents tab before submitting a
   prompt shows a timer counting time-since-launch. `BeginTurn()` already resets
   `root.StartedAt` at prompt submit (`tree.go:296`), and the renderer already
   treats a zero `StartedAt` as idle — `nodeElapsed()` returns the idle marker
   for `n.StartedAt.IsZero()` (`internal/tui/agents_model.go:756`). Only the
   pre-first-turn construction state is wrong.

2. **Model label frozen at the default.** The root node's `Label`/`Model` are set
   once from the startup alias via `NewAgentTree(alias, alias)`
   (`cmd/fuse/shell.go:133`). The `/model` handler updates only `m.alias`
   (`internal/tui/shell_model.go:743`) and never the tree root, so the tab always
   renders the initial model regardless of `/model` selection. `m.alias` is the
   live session alias (already read live in the prompt/status line, shell_model.go
   1317/1337). Change 0035 established that session-level settings are read live.

## Approach

- **Bug 1:** stop stamping `StartedAt` at construction. Leave it zero until the
  first `BeginTurn()`. No render change needed — the idle path already exists.
- **Bug 2:** add a tree method `SetRootModel(model string)` that updates the root
  node's `Label` and `Model` under the node lock and emits a tree update (mirrors
  how `BeginTurn` mutates the root). The `/model` handler calls it after updating
  `m.alias`. This keeps the tree the single source of the rendered label and
  matches the existing mutate-under-lock-then-Emit pattern rather than threading a
  live read into the render path.

## Tasks

### Task 1 — Root node starts idle (no pre-first-turn timer)

**Test first** (`internal/agent/tree_test.go`): a new tree's root node has a zero
`StartedAt` immediately after `NewAgentTree(...)`; after `BeginTurn()` the root's
`StartedAt` is non-zero. (If an existing test asserts a non-zero StartedAt at
construction, update it to the corrected idle expectation.)

**Implement:** in `NewAgentTree` (`internal/agent/tree.go:234`), remove the
`StartedAt: time.Now()` field from the root `AgentNode` literal so it defaults to
the zero `time.Time`. Do NOT touch child-node spawn stamping (out of scope).

**Verify:** root idle at construction, timer starts on first `BeginTurn()`;
`nodeElapsed()` renders the idle marker for the zero-value root.

### Task 2 — Tree root model label follows `/model`

**Test first** (`internal/agent/tree_test.go`): after `NewAgentTree("a", "a")`,
calling `tree.SetRootModel("b")` updates the root node's `Label` and `Model` to
`"b"` and emits a `TreeUpdate` for the root id. A no-op / empty-string guard if
warranted.

**Implement:** add `func (t *AgentTree) SetRootModel(model string)` to
`internal/agent/tree.go` — look up the root via `t.Node(t.rootID)`, nil-guard,
lock the node, set `Label` and `Model` to `model`, unlock, then
`t.Emit(TreeUpdate{NodeID: t.rootID})` (same shape as `BeginTurn`).

**Verify:** method updates label+model under lock and emits; existing tree tests
still green.

### Task 3 — Wire `/model` to update the tree root

**Test first** (`internal/tui/shell_model_test.go` or the existing `/model`
handler test): after attaching a tree via `WithTree` and dispatching `/model
NAME` for a resolvable model, the tree root's `Model`/`Label` equal `NAME`.
Reuse the existing model registry test double / resolvable-alias fixture used by
neighboring `/model` tests.

**Implement:** in the `/model` case (`internal/tui/shell_model.go:743`), after
`m.alias = name` and only on a successful `m.reg.Resolve(name)`, call
`m.tree.SetRootModel(name)` guarded by `if m.tree != nil`.

**Verify:** `/model` on a known model updates both `m.alias` and the tree root;
unknown model still rejected and leaves the root untouched; agents tab renders the
new model on the next tick.

## Final gate

`go build ./...` and `go test ./...` green across the repo (or the repo's
configured suite). Manual smoke (optional, noted for the merge gate): open the
agents tab before any prompt — timer shows idle; run `/model X`; open the tab —
label shows `X`.

## Out of scope

- Per-subagent child timers/labels (correct as stamped at spawn).
- Mid-turn model-switch execution semantics — this is display-only.
- Broader agents-tab UX rework.
