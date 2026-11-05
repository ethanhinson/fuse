---
id: 129
slug: 'per-loop-workspace-adapter-and-confined-file-tools'
title: 'Per-loop workspace adapter and confined file tools'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-05'
depends_on: [128]
stacked_on:
related: [65, 75, 77, 127]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-per-loop-workspace-adapter-and-confined-file-tools-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/per-loop-workspace-adapter-and-confined-file-tools'
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-per-loop-workspace-adapter-and-confined-file-tools-design.md](../../superpowers/specs/2026-11-03-per-loop-workspace-adapter-and-confined-file-tools-design.md) |
<!-- docket:artifacts:end -->

## Why

Hosted loops have two filesystems: `bash` runs in a per-tenant bind mount or a per-Pod emptyDir, while `read_file`/`write_file`/`edit_file`/`list_directory`/`grep` act on the loop server's own root filesystem with raw model paths and auto-approval. Files written by one tool are invisible to the other, a loop's files live on whichever replica ran it, two loops of one tenant share one writable tree, cross-tenant paths are reachable, and a process-wide scratch directory is advertised into every tenant's prompt. No type represents "this loop's workspace" and nothing durable records it.

## What changes

A `Workspace` seam with a local implementation keyed by (tenant, loop): file tools go through a confined `FileSystem` rooted at the workspace, `bash` mounts the same root, the scratch directory becomes `scratch/` inside it, and the workspace identity is recorded on the loop's stream so a resume re-opens the same one. `codeindex` runs inside the workspace or is not offered. Conformance suite for the seam; two-runtime test over a shared workspace parent.

## Out of scope

Moving workspace contents between nodes (0130 adds snapshot/restore and the Kubernetes volume spec); permission-mode changes; the orchestrator/shell bindings beyond compiling.
