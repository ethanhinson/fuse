---
id: 130
slug: 'workspaces-follow-a-loop-across-replicas'
title: 'Workspaces follow a loop across replicas'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: [129]
stacked_on:
related: [75, 128]
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

With 0129 a loop has an identity-bearing workspace, but its contents still live on the node that wrote them: a local directory on the replica, or an emptyDir that dies with the Pod. A move, a lease expiry, or a Pod timeout still loses the files. Callers that run long tasks need the workspace to be wherever the loop is.

## What changes

A snapshot/restore workspace provider over the artifact store (Postgres now, object storage later) that checkpoints the workspace at turn boundaries and on release, restores on open from another node, and a kubernetes workspace spec that lets the sandbox Pod mount the loop's workspace instead of an emptyDir. Conformance cases for restore fidelity; a two-runtime move test with real file contents.

## Out of scope

Live bidirectional sync during a turn; shared-volume (RWX PVC) provider beyond the config hook; workspace quotas and GC.
