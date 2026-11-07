---
id: 125
slug: 'stop-the-current-turn-of-an-interactive-loop-without-ending'
title: 'Stop the current turn in the shell without ending the session'
status: 'proposed'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [117]
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

In `fuse shell`, `/interrupt` requires a message: it cancels the running turn and immediately starts a new one. There is no way to just stop the agent and get the prompt back. Confirm the current key handling before building, since this was rewritten after the server tier was removed in PR #149.

## What changes

- Add a stop key or command in the shell that cancels the in-flight model call and tools and returns to the prompt.
- The conversation is kept, and the next prompt starts a normal turn.
- Tests cover a stop during a model call and during a tool call.

## Out of scope

Headless runs and steering semantics.
