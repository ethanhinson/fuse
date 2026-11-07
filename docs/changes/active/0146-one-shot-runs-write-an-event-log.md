---
id: 146
slug: 'one-shot-runs-write-an-event-log'
title: 'One-shot runs write an event log'
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

One-shot runs write no event log, so an unattended run leaves no record of what it did. Container runs and resume both need one.

## What changes

- One-shot runs write the same event log as the shell, under a location that can be set by flag or config so a container can point it at a mounted path.
- Tests cover a normal run, a failed run, and a child agent's events.

## Out of scope

Uploading logs, and any resume behaviour.
