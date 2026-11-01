---
id: 109
slug: 'type-based-guard-that-no-wire-request-carrying-secrets-is-ev'
title: 'Type-based guard that no wire request carrying secrets is ever formatted'
status: 'proposed'
priority: 'low'
type: 'feat'
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

The guard that StartLoopRequest and LoopSecret are never formatted keys on identifier names and runs only in one package; a formatting call elsewhere would print secret values via proto String, prototext or protojson.

## What changes

- go/types-based test that flags any *StartLoopRequest or *LoopSecret argument to fmt, log, slog or testing format calls.
- Cover sdk/fuse, cmd/fuse, internal/loopwire and internal/loopconnect.

## Out of scope

- Changing the wire types.
