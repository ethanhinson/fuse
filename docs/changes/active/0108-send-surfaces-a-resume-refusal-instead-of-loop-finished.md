---
id: 108
slug: 'send-surfaces-a-resume-refusal-instead-of-loop-finished'
title: 'Send surfaces a resume refusal instead of loop finished'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: [102]
discovered_from: [102]
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

Send discards Resume's error, so a client whose loop depended on loop-scoped secrets or a revoked operator secret gets FailedPrecondition: loop finished with no hint that the loop is unresumable by policy.

## What changes

- Map a harness ValidationError from Resume to FailedPrecondition carrying the ValidateHarnessResponse detail, as StartLoop does.
- Test with a loop-scoped-secret loop and a revoked operator secret.

## Out of scope

- Making such loops resumable.
