---
id: 3
slug: hitl-tool-approval
title: HITL tool-approval dialog
status: proposed
priority: medium
type: feat
created: 2026-08-04
updated: 2026-08-04
depends_on: [2]
related: [2]
discovered_from: [2]
adrs: []
spec:
trivial: false
auto_groomable: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

When running local or untrusted models the agent may call tools that read
sensitive paths, write files, or execute shell commands without the user's
knowledge.  A permission gate — modelled on the Claude Code and Cline UX —
lets the user approve, deny, or grant session-scoped allow-listing before
each sensitive action executes.

## What

* Tools declare whether they require approval (`NeedsApproval(args) (bool, reason)`).
* Before executing, the agent loop sends an `ApprovalRequestMsg` to the TUI
  instead of calling the tool immediately.
* The TUI renders a bottom-sheet overlay showing:
  - The tool name and what it is about to do (human-readable reason)
  - Numbered options: `1. Yes  2. Yes, allow <scope> this session  3. No`
  - `Esc to cancel · Tab to amend` footer
* The overlay blocks normal input; the agent goroutine blocks on a channel
  waiting for `ApprovalResponseMsg`.
* Approved responses optionally set a session-scoped allow-list so the same
  tool+scope is not re-prompted.

## Scope

* `tools.Tool` interface: add optional `Approver` interface (tools opt in).
* `agent/loop.go`: check approval before `Execute`; block on channel.
* `tui/events.go`: `ApprovalRequestMsg`, `ApprovalResponseMsg`.
* `tui/shell_model.go`: overlay state, numbered-option key handler, Esc/Tab.
* `tui/theme.go`: overlay styles (border, highlighted option, footer hint).
* Session allow-list: in-memory map on `ShellModel`, keyed by tool+scope.
* No persistence across sessions (Phase 1 scope).
