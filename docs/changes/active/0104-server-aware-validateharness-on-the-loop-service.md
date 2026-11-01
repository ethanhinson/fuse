---
id: 104
slug: 'server-aware-validateharness-on-the-loop-service'
title: 'Server-aware ValidateHarness on the loop service'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: [101]
discovered_from: [101]
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

Server-side rules (operator spec policy, tighten-only ceilings, skill and secret refs, operator-only auth types) are reported only when StartLoop refuses a spec. The static ValidateHarness has no operator config, so a client cannot check a spec against the server it will run on before starting a loop.

## What changes

- Add a ValidateHarness RPC to the loop service that runs the same resolve path as StartLoop (operator config, spec policy, tenant-visible secrets and skills) without starting a loop.
- Return the same ValidateHarnessResponse detail StartLoop uses.
- Go and TS SDK methods.

## Out of scope

- Changing the static harness validator.
- New policy rules.
