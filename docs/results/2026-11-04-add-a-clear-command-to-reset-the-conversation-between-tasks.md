<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0141 — Add a /clear command to reset the conversation between tasks](../changes/active/0141-add-a-clear-command-to-reset-the-conversation-between-tasks.md)**
<!-- docket:backlink:end -->

# Results: Add a `/clear` command to reset the conversation between tasks

## Summary

Implemented and shipped. The interactive shell (`internal/tui`) now has a
`/clear` built-in slash command that performs a clean-slate reset of the
conversation between tasks: it wipes the model-history slice (`m.history`),
the on-screen transcript (`m.lines`), pending tool-call UI bookkeeping
(`m.pendingCalls`, `m.inlineByLabel`, `m.inlineByNode`), and the per-session
recallable logs (`approvalLog`, `askLog`), while preserving the model alias,
registry, permission mode, verbose toggle, slash registry, builder, and agent
tree. It refuses while a turn is running and prints a confirmation line on
success.

## Changes

- `internal/tui/shell_model.go` — new `case "/clear":` arm in `handleSlash`:
  running-guard (refusal), drain pending approvals/asks through
  `drainApprovals`/`drainAsks` (cancel gate/tool goroutines — not a bare
  `= nil`, which would orphan `RespCh` and deadlock, the historical TUI
  freeze `drainApprovals` exists to prevent), reset history/lines/pending/
  inline/logs, redraw, confirm.
- `internal/tui/builtin_provider.go` — `/clear` entry so the slash completer
  surfaces it.
- `internal/tui/shell_model_test.go` — three tests:
  `TestClearCommand` (clean-slate + preserved alias/verbose/mode),
  `TestClearCommandRefusesWhileRunning` (guard),
  `TestClearCommandDrainsPendingApprovalsAndAsks` (no orphaned `RespCh`).

## Build evidence

`make test` — green at `7bebc49d1dbec5cfd7cda9448b505a211e7a4029` (62 packages).

## Review

A bounded read-only review (kimi) flagged one Major: the initial
implementation nil'd `m.approvals`/`m.asks` directly, which would orphan the
gate/tool goroutines blocked on each entry's `RespCh`. Fixed by draining
through `drainApprovals`/`drainAsks` before clearing the logs, and added
`TestClearCommandDrainsPendingApprovalsAndAsks` to cover it.

## PR

https://github.com/ethanhinson/fuse/pull/135 — `feat/add-a-clear-command-to-reset-the-conversation-between-tasks` → `main`.
