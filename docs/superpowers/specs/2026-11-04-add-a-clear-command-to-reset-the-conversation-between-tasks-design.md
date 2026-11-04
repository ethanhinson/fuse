<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0141 — Add a /clear command to reset the conversation between tasks](../../changes/archive/2026-11-04-0141-add-a-clear-command-to-reset-the-conversation-between-tasks.md)**
<!-- docket:backlink:end -->

# Spec: Add a `/clear` command to reset the conversation between tasks

## Problem

The interactive shell (`internal/tui`) accumulates every prior turn into its
model-history slice — the `history []model.Message` field on `ShellModel`
(`internal/tui/shell_model.go`) — for the life of the process. `startPrompt`
appends the user line and captures `m.history` into the closure that builds and
runs the agent turn, so the model sees the full conversation on every turn.

There is no user-driven reset:

- `Ctrl+L` (`shell_model.go`, the `tea.KeyCtrlL` case) only wipes the *visible
  transcript* (`m.lines`, `m.pendingCalls`, the inline-block maps). It
  deliberately leaves `m.history` untouched, so the model keeps every prior
  turn's context while the screen goes blank. It is a screen-clear, not a
  conversation-clear.
- The automatic segment compaction (`internal/segment`, the relevance and
  segment gateways) trims *old tool results* within a single long task to bound
  context growth. It does not reset the user/assistant conversation, and it is
  not user-triggered.

So the only way to start a fresh task without cross-task context bleed is to
quit and restart `fuse` — which also discards the model alias, permission mode,
registry, and MCP configuration the user has set up for the session.

## Outcome

A user can type `/clear` in the interactive shell to start a fresh
conversation — empty model history, blank screen, empty recallable logs —
without restarting the process, keeping their model alias, permission mode,
registry, verbose setting, and MCP wiring.

## Design

### Command surface

Add `/clear` as a built-in slash command:

1. A `case "/clear":` arm in `handleSlash` (`internal/tui/shell_model.go`),
   placed near the other transcript/commands arms and before the
   registry-lookup fallback.
2. A new entry in `NewBuiltinProvider()` (`internal/tui/builtin_provider.go`)
   so the slash completer surfaces it:
   `{Command: "/clear", Description: "Clear the conversation history and screen (start a fresh task)", Kind: KindBuiltin, expand: func() string { return "/clear" }}`

The completer already enumerates `BuiltinProvider` entries, so no completer
changes are needed.

### Clean-slate reset

The `/clear` case resets the session-scoped conversation state, leaving
configuration alone. Specifically it sets to nil/empty:

| Field | Reset? | Rationale |
| --- | --- | --- |
| `m.history` | yes | the `[]model.Message` sent to the model — the actual conversation context |
| `m.lines` | yes | the on-screen transcript scrollback (same field `Ctrl+L` already clears) |
| `m.pendingCalls` | yes | in-flight tool-call UI bookkeeping (same field `Ctrl+L` already clears) |
| `m.inlineByLabel`, `m.inlineByNode` | yes | inline-block tracking keyed into `m.lines` (same fields `Ctrl+L` already clears) |
| `m.approvalLog` | yes | the per-session log `/approvals` recalls |
| `m.askLog` | yes | the per-session log `/questions` recalls |
| `m.approvals` | yes | the pending-approval FIFO (guarded — see below) |

It preserves (does **not** touch):

| Field | Kept? | Rationale |
| --- | --- | --- |
| `m.alias` | yes | the active model — the point is to not restart the shell |
| `m.reg` | yes | the model registry |
| `m.sessionMode` | yes | the permission mode (`Shift+Tab`/`/mode`) |
| `m.verbose` | yes | the verbose tool-output toggle |
| `m.slashReg` | yes | the slash command registry (skills/MCP) |
| `m.build` | yes | the `AgentBuilder` |
| `m.tree` | yes | the live agent tree (a clear is between tasks; a running tree is refused — see below) |

After the reset, call `m.refreshViewport(true)` to redraw the (now empty)
viewport, then append a confirmation line, e.g.
`m.appendLine(headerStyle.Render("conversation cleared"))`, so the user gets
visible feedback that the reset landed (unlike a silent wipe).

### Refusal while a turn is running

`/clear` must refuse while a turn is in flight, because resetting `m.history`
mid-turn would race the goroutine that already captured it in the running
closure (`startPrompt` captures `history := m.history` into the run closure).
The existing `m.running` flag is the guard. On refusal, append a line such as
`m.appendLine(headerStyle.Render("cannot clear while a turn is running — wait or /interrupt first"))` and return.

This mirrors the guard the shell already uses elsewhere (input is suppressed
while `m.running`), so the clearing path reuses the same predicate rather than
introducing a new lock.

### Relationship to `Ctrl+L`

`Ctrl+L` stays a screen-only wipe. `/clear` is a superset that *also* resets the
model history and the recallable logs — the two commands stay distinct so
`Ctrl+L`'s existing "clear the screen, keep the context" behavior is preserved
and a user who only wants a clean scrollback keeps it.

## Tests

Mirror the existing `TestCtrlLClears` test (`internal/tui/shell_model_test.go`)
for the new command:

1. **`TestClearCommand`** — populate `m.history`, `m.lines`, `m.approvalLog`,
   and `m.askLog`; send `/clear`; assert all four are empty, `m.alias` is
   unchanged, the model registry is unchanged, and a confirmation line was
   appended.
2. **`TestClearCommandRefusesWhileRunning`** — set `m.running = true`; send
   `/clear`; assert `m.history` is **not** reset and a refusal line was
   appended.

Add the new entry to the builtin provider snapshot if `NewBuiltinProvider` is
snapshot-tested (search for an existing golden/test before adding).

## Out of scope

- Resetting the agent tree (`m.tree`) or the blackboard state — a clear is
  *between* tasks; a running tree is refused, not reset.
- A `/clear` for the one-shot CLI or loop-server paths (they have no persistent
  shell history).
- Automatic context clearing on perceived task boundaries.
- A confirmation prompt before clearing (the command is deliberately
  immediate, like `Ctrl+L`).
