---
id: 152
slug: 'os-sandbox-for-local-bash'
title: 'OS sandbox for local bash'
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

Local bash needs Docker for containment, and refuses to run without it. Vendor CLIs use Seatbelt and Landlock, which need no daemon and start instantly.

## What changes

- A Seatbelt handler on macOS and a Landlock handler on Linux limit writes to the workspace and temp, with network configurable.
- Selection falls back to refusal, never to running uncontained.
- Tests prove writes outside the workspace fail.

## Out of scope

Removing the container handler.
