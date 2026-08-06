---
id: 32
slug: shell-mode-switcher
title: Shell permission-mode switcher — cycle smart/auto in the TUI, with a visible mode indicator
status: proposed
priority: high
type: feat
created: 2026-08-05
updated: 2026-08-05
depends_on: [17]
related: [17, 3, 10]
discovered_from: [17]
adrs: [6]
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

Change 0017 shipped the auto-mode engine, but it is reachable only via
`permissions.mode: auto` in `~/.fuse/config.yml` — deliberately not from the
repo-local file (ADR-0006), and with no runtime surface at all. First real
use immediately surfaced the gap: the human built the branch, ran the shell,
and was still prompted for every action with no way to see or change the
active mode. Claude Code's Shift+Tab mode cycling is the reference
ergonomic: the permission mode should be visible in the shell and switchable
mid-session without editing YAML and restarting.

## What changes

- A visible mode indicator in the shell TUI (status/input line) showing the
  active permission mode (smart / auto / prompt-all / off).
- A keybinding (Shift+Tab, mirroring Claude Code, or the nearest
  terminal-portable equivalent) that cycles between modes — at minimum
  smart ("standard") and auto; plus a slash command (e.g. `/mode auto`) as
  the discoverable, always-portable path.
- Mid-session switching semantics: the change applies to the root gate and
  is inherited by newly spawned child gates; define what happens to
  already-running children and to the session approval cache and escalation
  valve on a switch.
- Startup default remains the configured mode; the switcher is a session
  override, not a config write. The ADR-0006 trust boundary is unaffected
  (this is a human at the keyboard, not a repo file).

## Out of scope

- One-shot (`fuse run`) mode flags beyond the existing `--approve-all`.
- Persisting the switched mode back to config.
- New permission modes; this only surfaces the existing four.

## Open questions

- Is Shift+Tab (CSI `Z` / backtab) reliably deliverable through Bubble Tea
  across common terminals, or is a different chord safer as the default?
- Does switching to a stricter mode mid-run invalidate session-cache grants
  made under a looser mode?
- Should auto mode entered via the switcher require the classifier to be
  configured, or silently run with the deterministic layers plus
  fail-closed asks (current behavior when classifier_model is unset)?

## Reconcile log
