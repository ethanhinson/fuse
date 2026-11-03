---
id: 125
slug: 'stop-the-current-turn-of-an-interactive-loop-without-ending'
title: 'Stop the current turn of an interactive loop without ending it'
status: 'proposed'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
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

Callers can steer a running turn (`Send` with `interrupt`, which cancels the in-flight model call or tool batch and starts a new turn with the steer text) or cancel the whole loop (`CancelLoop`). They cannot stop the current turn and leave the interactive loop parked, waiting for the next message. A user-facing Stop therefore has to end the whole session.

## What changes

- Add a way to stop the current turn and park the loop: either an interrupt variant with no follow-up message, or a `CancelTurn` RPC.
- It cancels the in-flight model call and tools, then emits `loop.parked` with a `stopped` reason.
- The next `Send` starts a normal turn.
- Scope: same owner and tenant rules as Send.
- Tests cover a stop during a model call and during a tool call.

## Out of scope

Changing CancelLoop or the steering semantics.
