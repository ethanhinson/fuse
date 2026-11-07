---
id: 119
slug: 'send-to-a-loop-cancelled-on-another-replica-says-loop-cancel'
title: 'Send to a loop cancelled on another replica says loop cancelled'
status: 'killed'
priority: 'low'
type: 'fix'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [117, 108]
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

Suppose another replica cancels a loop, and this replica still holds that loop in memory as finished. Send then answers "loop finished" instead of "loop cancelled". The loop never runs again, so this is safe, but callers get a misleading error in a cross-replica edge case.

## What changes

- In `Handler.Send`, take the existing `ErrLoopFinished` branch: when the retry after Resume still returns `ErrLoopFinished`, consult `runtime.CancelReader.IsCancelled`, and map the result to FailedPrecondition "loop cancelled".
- Test it with two replicas.

## Out of scope

Changing the resume semantics.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared multi-tenant server or harness specs. Merged work (PR #147) targets multi-replica Send, which no longer applies.
