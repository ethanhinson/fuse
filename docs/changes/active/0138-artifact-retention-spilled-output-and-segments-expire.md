---
id: 138
slug: 'artifact-retention-spilled-output-and-segments-expire'
title: 'Artifact retention: spilled output and segments expire'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [128]
discovered_from: [128]
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

Spilled tool output and compaction segments live in the local artifact store under `~/.fuse`. The old 7-day spill sweep was removed, so nothing deletes artifacts and the directory grows without bound on a long-lived machine.

## What changes

- A configurable TTL for the artifact store.
- A sweep at startup, in the same style as the session-log and scratch sweeps, that is safe when two fuse processes run at once.
- An expired ref reads as not found with a clear message.
- Tests cover expiry.

## Out of scope

Address pinning and object storage.
