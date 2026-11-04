---
id: 141
slug: 'add-a-clear-command-to-reset-the-conversation-between-tasks'
title: 'Add a /clear command to reset the conversation between tasks'
status: 'in-progress'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-04'
updated: '2026-11-04'
depends_on: []
stacked_on:
related: [81]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks-design.md'
plan: 'docs/superpowers/plans/2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks.md'
results: 'docs/results/2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks.md'
auto_groomable:
hold:
branch_prefix:
branch: 'feat/add-a-clear-command-to-reset-the-conversation-between-tasks'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks-design.md](../../superpowers/specs/2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks-design.md) |
| Plan | [2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks.md](../../superpowers/plans/2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks.md) |
| Results | [2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks.md](../../results/2026-11-04-add-a-clear-command-to-reset-the-conversation-between-tasks.md) |
<!-- docket:artifacts:end -->

## Why

The interactive shell accumulates every prior turn into its model-history slice across tasks within a single session. The only reset today is quitting and restarting fuse: Ctrl+L wipes the visible scrollback but deliberately leaves the history untouched, so the model keeps seeing prior tasks' turns — wasting context window and risking cross-task bleed when a user moves on to an unrelated task. A manual, whole-conversation reset lets a user start a fresh task without restarting the shell.

## What changes

Add a `/clear` slash command to the interactive shell that performs a clean-slate reset: wipe the on-screen transcript, reset the model-history slice (the `[]model.Message` sent to the model each turn), and clear the per-session recallable logs (`approvalLog`, `askLog`). It preserves the model alias, permission mode, model registry, and verbose setting so the user keeps their session configuration. It refuses while a turn is running and prints a confirmation line on success. It complements the automatic segment compaction (which trims old tool results within a single long task) rather than replacing it.

## Out of scope

Resetting the agent tree or blackboard state; resetting the model registry or permission mode; a one-shot CLI flag; automatic context clearing on task boundaries.

## Reconcile log

### 2026-11-04

Re-read internal/tui/shell_model.go and internal/tui/builtin_provider.go against the spec. Ctrl+L (tea.KeyCtrlL, ~line 808) clears m.lines/m.pendingCalls/m.inlineByLabel/m.inlineByNode and calls refreshViewport(true) but leaves m.history untouched — matches the spec premise. startPrompt (~line 1173) appends to m.history and captures history := m.history into the run closure, so the running guard is correct. NewBuiltinProvider has no /clear entry; handleSlash has no /clear case. No ADRs needed (small, self-contained TUI feature). Design holds as written.
