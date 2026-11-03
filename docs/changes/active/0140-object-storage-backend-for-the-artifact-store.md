---
id: 140
slug: 'object-storage-backend-for-the-artifact-store'
title: 'Object-storage backend for the artifact store'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [128, 130]
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

The artifact store has local filesystem, Postgres and in-memory backends. Large spilled outputs and segments belong in object storage (S3-compatible, GCS) for deployments that keep the event store small.

## What changes

- An S3-compatible backend (and optionally GCS) implementing `artifact.Store`, passing `artifacttest.RunStoreConformance` against a local MinIO in CI.
- Configuration and docs for choosing it.

## Out of scope

Workspace snapshots (#130 owns those).
