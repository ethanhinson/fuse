---
id: 128
slug: 'loop-artifact-store-adapter-for-spilled-tool-output-and-comp'
title: 'Loop artifact store adapter for spilled tool output and compaction segments'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [30, 43, 46, 127]
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

Oversized tool results are truncated with a recovery file written to a process-global directory, and compaction segments are written to a local sessions directory. Hosted loops never set either, so large outputs are lost outright; enabling them as they are would write one replica's absolute path into the durable transcript, unreadable after a move and readable across tenants. There is no interface for spill; the segment sink has only a local-file implementation.

## What changes

One `ArtifactStore` seam, scoped by (tenant, loop), with a local-filesystem implementation and a Postgres implementation behind the existing `pgstore` build tag, plus a conformance suite. Spill and segments become artifacts addressed by opaque refs; `read_file`/`grep`/`segment_read` resolve refs through the store. The loop server wires the store per loop; the package-level `SetSpillDir`/`SetSegmentsDir` globals are retired.

## Out of scope

Object-storage (S3/GCS) implementation beyond leaving the interface blob-shaped; retention/purge policy; changing truncation thresholds.
