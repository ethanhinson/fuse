---
id: 121
slug: 'go-sdk-cancel-a-loop'
title: 'Go SDK: cancel a loop'
status: 'killed'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [117]
discovered_from: [117]
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

The networked loop server has a `CancelLoop` RPC and the TypeScript SDK exposes `client.cancel`, but the Go SDK has no cancel method. Go callers must hand-roll the Connect call.

## What changes

- Add a Go SDK method that calls `CancelLoop` and returns the outcome, `was_live`, `terminal_seq` and reason.
- Treat `loop.cancelled` as a completion event in the SDK's observe helpers.
- Tests and a README entry.

## Out of scope

New server behaviour.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared multi-tenant server or harness specs. Merged work (PR #146) targets the Go SDK, which no longer applies.
