---
id: 38
slug: turn-budget-retirement
title: Retire the interactive turn cap — unlimited shell turns, headless backstop, doom-loop detection
status: proposed
priority: critical
type: fix
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [17, 35]
discovered_from: [17]
adrs: []
spec:
plan:
results:
trivial: true
auto_groomable:
branch: feat/live-mode-switch
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

The agent loop hard-caps every run at `max_turns` (default 25,
`internal/agent/loop.go:133`, `ErrMaxTurns`). With auto mode shipped, long
unattended runs are the point — and the first real long run died at
"agent: max turns reached". Survey of peer agents: Claude Code has no
interactive cap (`--max-turns` is headless-only opt-in); Cline shipped an
equivalent cap and later removed it; OpenCode uses doom-loop *detection*
(repeated identical calls routed to approval) instead of counting honest
progress; Aider bounds only its fix-retry loop. A blunt turn counter
punishes productive runs while the failure it guards against — a stuck
loop — is better caught by shape than by count.

## What changes (design settled with the human, 2026-08-06)

- **Interactive shell: unlimited by default.** `max_turns` unset ⇒ no cap in
  the shell path. Presence detection via a `*int` in the raw config (the
  `session_allow` pattern from 0017/T10) — the hardcoded 25 default and the
  `<=0 ⇒ 25` coercion in `agent.New` are retired.
- **Headless backstop stays.** One-shot `fuse run`, non-TTY, mcp-server, and
  research-probe default to a generous backstop (100 turns) when `max_turns`
  is unset — nothing can interrupt a runaway there.
- **Explicit config wins everywhere**: `max_turns: N` (N>0) caps every
  context; `max_turns: 0` means explicitly unlimited everywhere (the
  scripted-use footgun, like --approve-all — visible and deliberate).
- **Doom-loop detection** (both contexts): 3 consecutive byte-identical tool
  calls (same name + same args) trips the detector — interactive: the call
  is forced through the approval prompt with a "possible loop" preview
  regardless of mode; non-interactive: abort with a clear structured error
  naming the repeated call. Counter resets on any differing call.
- Rides **PR #16** (`feat/live-mode-switch`) at the human's direction — one
  merge completes the auto-mode long-run experience; `pr:` set to #16.

## Out of scope

- Token/spend budgeting; context-window compaction.
- Per-project `max_turns` overrides (the `projects:` map stays
  permissions-only for now).

## Reconcile log
