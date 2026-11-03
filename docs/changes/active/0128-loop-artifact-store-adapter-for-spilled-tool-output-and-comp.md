---
id: 128
slug: 'loop-artifact-store-adapter-for-spilled-tool-output-and-comp'
title: 'Loop artifact store adapter for spilled tool output and compaction segments'
status: 'implemented'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [30, 43, 46, 127]
discovered_from: []
adrs: [80]
spec: 'docs/superpowers/specs/2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp-design.md'
plan: 'docs/superpowers/plans/2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp.md'
results: 'docs/results/2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'feat/loop-artifact-store-adapter-for-spilled-tool-output-and-comp'
pr: 'https://github.com/ethanhinson/fuse/pull/133'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp-design.md](../../superpowers/specs/2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp-design.md) |
| Plan | [2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp.md](../../superpowers/plans/2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp.md) |
| Results | [2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp-results.md](../../results/2026-11-03-loop-artifact-store-adapter-for-spilled-tool-output-and-comp-results.md) |
| ADRs | [ADR-0080](../../adrs/0080-loop-artifacts-are-addressed-by-tenant-scoped-refs-never-fil.md) |
<!-- docket:artifacts:end -->

## Why

Oversized tool results are truncated with a recovery file written to a process-global directory, and compaction segments are written to a local sessions directory. Hosted loops never set either, so large outputs are lost outright; enabling them as they are would write one replica's absolute path into the durable transcript, unreadable after a move and readable across tenants. There is no interface for spill; the segment sink has only a local-file implementation.

## What changes

One `ArtifactStore` seam, scoped by (tenant, loop), with a local-filesystem implementation and a Postgres implementation behind the existing `pgstore` build tag, plus a conformance suite. Spill and segments become artifacts addressed by opaque refs; `read_file`/`grep`/`segment_read` resolve refs through the store. The loop server wires the store per loop; the package-level `SetSpillDir`/`SetSegmentsDir` globals are retired.

## Out of scope

Object-storage (S3/GCS) implementation beyond leaving the interface blob-shaped; retention/purge policy; changing truncation thresholds.

## Reconcile log

### 2026-11-03

Unchanged against main 5eb0925: spill/segment globals, nil loop-server SegmentSink and CLI SetSpillDir call sites still match the spec pointers. Change 126 (fenced ownership) builds in parallel; this branch rebases onto origin/main before the PR.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 98ca828348c24391804fb082db97a8905827151c
ran_at:   2026-11-03T10:56:56Z
<!-- docket:build-evidence:end -->
