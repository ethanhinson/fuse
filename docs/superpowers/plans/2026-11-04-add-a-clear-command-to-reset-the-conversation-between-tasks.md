<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0141 — Add a /clear command to reset the conversation between tasks](../../changes/active/0141-add-a-clear-command-to-reset-the-conversation-between-tasks.md)**
<!-- docket:backlink:end -->

# Plan: Add a `/clear` command to reset the conversation between tasks

## Task 1 — Add the `/clear` builtin entry

`internal/tui/builtin_provider.go` — add to `NewBuiltinProvider().entries`:
```go
{Command: "/clear", Description: "Clear the conversation history and screen (start a fresh task)", Kind: KindBuiltin, expand: func() string { return "/clear" }},
```

## Task 2 — Add the `/clear` case to `handleSlash`

`internal/tui/shell_model.go` — new `case "/clear":` arm in `handleSlash`'s switch, before the registry-lookup fallback:
- Guard: refuse while `m.running` (append refusal line, return).
- Drain pending approvals/asks via `m.drainApprovals()` / `m.drainAsks()` (cancel gate/tool goroutines — not a bare `= nil`, which orphans `RespCh`).
- Reset: `m.history`, `m.lines`, `m.pendingCalls`, `m.inlineByLabel`, `m.inlineByNode`, `m.approvalLog`, `m.askLog` → nil/empty.
- Redraw + confirm: `m.refreshViewport(true)`, append "conversation cleared".
- Preserve: `m.alias`, `m.reg`, `m.sessionMode`, `m.verbose`, `m.slashReg`, `m.build`, `m.tree`.

## Task 3 — Tests

`internal/tui/shell_model_test.go`:
- `TestClearCommand` — history/lines/logs cleared, alias/verbose/mode preserved, confirmation line.
- `TestClearCommandRefusesWhileRunning` — guard: history unchanged, refusal line.
- `TestClearCommandDrainsPendingApprovalsAndAsks` — pending approval/ask `RespCh` drained (not orphaned), logs cleared.

## Task 4 — Build gate

`make test` green at the feature head.
