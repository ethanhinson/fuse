---
id: 129
slug: 'per-loop-workspace-adapter-and-confined-file-tools'
title: 'Per-loop workspace adapter and confined file tools'
status: 'killed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: [128]
stacked_on:
related: [65, 75, 77, 127]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-per-loop-workspace-adapter-and-confined-file-tools-design.md'
plan: 'docs/superpowers/plans/2026-11-03-per-loop-workspace-adapter-and-confined-file-tools.md'
results: 'docs/results/2026-11-04-0129-per-loop-workspace-adapter-and-confined-file-tools.md'
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-per-loop-workspace-adapter-and-confined-file-tools-design.md](../../superpowers/specs/2026-11-03-per-loop-workspace-adapter-and-confined-file-tools-design.md) |
| Plan | [2026-11-03-per-loop-workspace-adapter-and-confined-file-tools.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/plans/2026-11-03-per-loop-workspace-adapter-and-confined-file-tools.md) |
| Results | [2026-11-04-0129-per-loop-workspace-adapter-and-confined-file-tools.md](https://github.com/ethanhinson/fuse/blob/docket/docs/results/2026-11-04-0129-per-loop-workspace-adapter-and-confined-file-tools.md) |
<!-- docket:artifacts:end -->

## Why

Hosted loops have two filesystems: `bash` runs in a per-tenant bind mount or a per-Pod emptyDir, while `read_file`/`write_file`/`edit_file`/`list_directory`/`grep` act on the loop server's own root filesystem with raw model paths and auto-approval. Files written by one tool are invisible to the other, a loop's files live on whichever replica ran it, two loops of one tenant share one writable tree, cross-tenant paths are reachable, and a process-wide scratch directory is advertised into every tenant's prompt. No type represents "this loop's workspace" and nothing durable records it.

## What changes

A `Workspace` seam with a local implementation keyed by (tenant, loop): file tools go through a confined `FileSystem` rooted at the workspace, `bash` mounts the same root, the scratch directory becomes `scratch/` inside it, and the workspace identity is recorded on the loop's stream so a resume re-opens the same one. `codeindex` runs inside the workspace or is not offered. Conformance suite for the seam; two-runtime test over a shared workspace parent.

## Out of scope

Moving workspace contents between nodes (0130 adds snapshot/restore and the Kubernetes volume spec); permission-mode changes; the orchestrator/shell bindings beyond compiling.

## Reconcile log

### 2026-11-05

Re-read internal/tools/{read,write,edit,list,grep}.go, registry.go, cmd/fuse/scratch.go, cmd/fuse/run.go, cmd/fuse/sandbox.go, internal/runtime/*.go against the spec. File tools still call raw os.* on the model path (write.go:47/50, edit.go:46/59, list.go:54, grep.go:144/232) and register zero-value constructors (registry.go:253-260 DefaultTools(*sandbox.Service)). sessionScratchDir/scratchOnce/appendScratchBlock exist in cmd/fuse/scratch.go. No workspace.Provider field in runtime.Deps yet. sandbox.Pool keyed by tenant/principal, no per-loop mount source; TenantRoots is the fallback. The Go 1.24 os.Root API the spec leans on is available (go.mod toolchain >= 1.24). Design holds as written — the new internal/workspace package, file-tool FileSystem signature change, runtime.Deps.Workspaces wiring, conformance suite, and two-runtime resume test are all greenfield against current main (now at 0cd6a74 with #127 merged). No ADRs needed beyond the workspace-adapter one the spec names in acceptance criterion 8.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared server. Its premise is hosted loops with two filesystems. In one container per task the container is the boundary. PR #136 closed unmerged; branch kept.
