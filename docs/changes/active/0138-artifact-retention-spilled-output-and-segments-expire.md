---
id: 138
slug: 'artifact-retention-spilled-output-and-segments-expire'
title: 'Artifact retention: spilled output and segments expire'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
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

Loop artifacts (spilled tool output and compaction segments) now live in an artifact store addressed by tenant-scoped refs. The old 7-day spill sweep was removed with the process-global spill directory, so nothing deletes artifacts and the store grows without bound.

## What changes

- A retention policy on the artifact store: a configurable TTL, plus deletion when a loop's stream is purged.
- A sweeper that runs safely on many replicas (claimed work, idempotent deletes), for every backend.
- An expired ref reads as not found with a clear message.
- Conformance tests cover expiry.

## Out of scope

Object-storage backends (a separate change).
