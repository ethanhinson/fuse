---
id: 153
slug: 'user-hooks-around-tool-calls'
title: 'User hooks around tool calls'
status: 'proposed'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-07'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec:
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

Teams use hooks to enforce checks like formatting and blocked paths without relying on the model. Fuse has none. Evidence for hooks improving outcomes is thin, so this follows the items above.

## What changes

- Config declares commands that run before and after tool calls and at stop.
- A pre-hook can block a call and return a reason to the model.
- Failures and timeouts are reported.
- Tests cover block, allow, and timeout.

## Out of scope

A plugin system.
