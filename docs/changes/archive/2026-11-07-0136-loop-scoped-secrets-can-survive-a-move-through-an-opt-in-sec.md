---
id: 136
slug: 'loop-scoped-secrets-can-survive-a-move-through-an-opt-in-sec'
title: 'Loop-scoped secrets can survive a move through an opt-in secret store adapter'
status: 'killed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: [126]
stacked_on:
related: [102, 108, 110]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-loop-scoped-secrets-can-survive-a-move-through-an-opt-in-sec-design.md'
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
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-loop-scoped-secrets-can-survive-a-move-through-an-opt-in-sec-design.md](../../superpowers/specs/2026-11-03-loop-scoped-secrets-can-survive-a-move-through-an-opt-in-sec-design.md) |
<!-- docket:artifacts:end -->

## Why

Loop-scoped secret values are memory-only by design, so any loop whose spec relies on them cannot be resumed on another replica: lease-expiry failover, drain hand-off and idle-reap resume all fail re-resolution. That was the right first cut for confidentiality, but callers running hosted loops over a fleet need failover for those loops too, under an operator switch.

## What changes

A `LoopSecretStore` seam (memory-only default = today; Postgres implementation envelope-encrypted with an operator key and bound to the loop's tenant and hosts), used by resume when the operator enables it, with explicit deletion at loop end and on cancel. ADR-0076 is superseded for the opt-in case only. Conformance suite; two-runtime failover test with a secret-bearing spec.

## Out of scope

Changing precedence or host-binding rules; exposing values through any RPC; a KMS integration beyond the key seam.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared multi-tenant server or harness specs. Only needed for the shared server or fleet.
