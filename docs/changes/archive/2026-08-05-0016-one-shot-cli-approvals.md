---
id: 16
slug: one-shot-cli-approvals
title: Remove the AlwaysApprove one-shot bypass; surface approvals at the CLI
status: killed
priority: medium
type: feat
created: 2026-08-05
updated: 2026-08-05
depends_on: []
related: [3, 12]
discovered_from: []
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

One-shot mode (`fuse run`) wires `permissions.AlwaysApprove` into the permission
gate (`cmd/fuse/main.go` root agent build; also the child-agent builds there and
in `cmd/fuse/shell.go`). The gate's disabled list and mode resolution still run,
but anything that would normally prompt a human is silently approved — so a
one-shot invocation can execute arbitrary bash and file writes with no human
gate, regardless of smart-mode / prompt-all configuration. The permission policy
a user configures should mean the same thing in every entry point.

## What changes

- Remove the blanket `AlwaysApprove` default from the one-shot path so the
  configured permission mode is actually enforced.
- Add a CLI approval surface for one-shot runs: when the gate needs a decision,
  surface the pending approval on the terminal (TTY y/N-style prompt with the
  existing preview line, honoring allow-for-session) instead of auto-approving.
- Define the non-interactive posture (no TTY: piped stdin, CI): deny-by-default
  with a clear error, with an explicit opt-in flag to restore approve-all for
  scripted use.

## Out of scope

- The interactive shell's TUI approval flow (already exists).
- The MCP server's socket-based HITL relay (`internal/hitl`) — already routes
  approvals to the parent when a socket is present.

## Open questions

- Non-TTY posture: hard deny vs. configurable; name of the explicit opt-in flag
  (`--approve-all`? `--dangerously-skip-permissions`-style?).
- Do one-shot child/subagent tool calls prompt through the same terminal
  channel, or inherit the parent's session cache only? (Child builds currently
  pass `AlwaysApprove` too.)
- `fuse mcp-server` without a HITL socket currently falls back to
  `AlwaysApprove` — same treatment or separate change?

## Reconcile log

## Why killed

Subsumed by change 0017 (auto mode): AlwaysApprove removal, one-shot CLI approval surface, and non-TTY posture are all in 0017's spec (D8).
