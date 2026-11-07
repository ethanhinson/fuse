---
id: 147
slug: 'hard-limits-and-clean-shutdown-for-headless-runs'
title: 'Hard limits and clean shutdown for headless runs'
status: 'proposed'
priority: 'high'
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

Headless runs have no total deadline since the hosted default was removed, and the one-shot path has no signal handling, so a stopped container dies mid-write with no final state. max_run_tokens exists but runs need a wall-clock limit too.

## What changes

- A max_run_duration setting and flag ends a run cleanly when reached.
- SIGTERM and SIGINT cancel the running model call and tools, flush logs, and exit nonzero with a clear reason.
- Hitting any limit is reported as a limit stop, never as success.
- Tests cover the deadline, each signal, and the token cap.

## Out of scope

Dollar budgets, which belong in the gateway.
