---
id: 82
slug: 'per-loop-workspace-adapter-and-confined-file-tools'
title: 'Per-loop workspace adapter and confined file tools'
status: 'Accepted'
date: '2026-11-05'
supersedes: []
reverses: []
relates_to: []
change:
---

## Context

The file tools (read_file, write_file, edit_file, list_directory, grep) called os.* on the raw model path and were registered unconditionally, and the sandbox gate workspace root was the server process cwd. bash ran in ~/.fuse/workspaces/<tenant> (keyed by tenant, never by loop) while read_file operated on the process cwd, so bash and read_file disagreed about what the working directory was. The scratch directory was one process-global os.MkdirTemp advertised as an absolute host path in every system prompt. Cross-tenant reads and writes were one path away, and a symlink planted inside the workspace escaped to the host filesystem.

## Decision

Every loop opens one confined workspace (internal/workspace.Provider) keyed by (tenant, loop). The file tools take a workspace.FileSystem (every path workspace-relative; .., absolute, and symlink escapes are ErrEscape, enforced by the kernel on *os.Root for the local provider). bash container substrate bind-mounts the workspace MountSource at /workspace so bash and read_file agree. The workspace identity is recorded as a loop.workspace event on the loop durable stream right after loop.harness, so a resume on any replica re-opens the same workspace (refusing a provider-name mismatch). codeindex is registered only when the workspace has a MountSource and the sandbox can run it; otherwise the model never sees a tool that answers about the wrong filesystem. The process-global scratch dir is retired; scratch/ is workspace-relative, advertised as scratch/ (never an absolute host path) in the system prompt.

## Consequences

A hosted loop can read and write exactly one tree (its own) and no path it can name reaches another loop or another tenant files. A resume on a second runtime over the same provider parent re-opens the same workspace by the ID recorded on the stream. The system prompt contains no absolute host path. The workspace is never deleted in this change (Release is a no-op for the local provider); copying workspace contents between nodes, the kubernetes volume spec, and permission-mode semantics are out of scope (change 0130).

## Alternatives considered

Keep the file tools on raw os.* and rely on the permission gate path scoping alone: rejected. Per-tenant workspace (not per-loop): rejected. Mount the workspace at a per-loop path under /workspace rather than at /workspace: rejected.
