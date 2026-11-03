---
id: 80
slug: 'loop-artifacts-are-addressed-by-tenant-scoped-refs-never-fil'
title: 'Loop artifacts are addressed by tenant-scoped refs, never filesystem paths'
status: 'Accepted'
date: '2026-11-03'
supersedes: []
reverses: []
relates_to: [24, 30, 31]
change: 128
---

## Context

Oversized tool output (spill) and pre-compaction transcript segments were written to process-global local directories (SetSpillDir/SetSegmentsDir), and the durable tool.result embedded a replica-local absolute path. Hosted loops never set those dirs, so output was lost; a moved or resumed loop on another replica could not read the path; and a path is a potential cross-tenant read.

## Decision

One `artifact.Store` seam (Put/Open/List), scoped by (tenant, loop, kind, id) and addressed by opaque refs `art://<tenant>/<loop>/<kind>/<id>`. Durable payloads carry only refs.

Backends: `artifact/fsstore` (local, ~/.fuse/artifacts, gzip at rest, refuses unsafe or case-colliding ids); `artifact/pgstore` (Postgres `artifacts` table behind the `pgstore` build tag, gzip bodies, 16 MiB cap); and an in-memory `MemStore` (the default when runtime.Deps.Artifacts is nil, 64 MiB per-loop cap). The composition root selects pgstore when a Postgres DSN is configured, fsstore otherwise.

Each loop's tool registry is bound to (store, tenant, loop); read_file/grep/list_directory/segment_read resolve refs only through that binding. A wrong-tenant or wrong-loop ref reads as ErrNotFound, indistinguishable from missing, and store errors never surface host paths in tool output. All backends must pass `artifacttest.RunStoreConformance`. The package-level globals (SetSpillDir/SetSegmentsDir) are deleted.

## Consequences

Loops resumed on another runtime sharing the store recover spills and segments. Tenant isolation is enforced at the seam rather than by callers. The interface is blob-shaped, so an object-storage backend can be added later. There is no retention/purge yet, so artifacts accumulate. The segment index is a rewritten artifact that assumes a single writer (the loop's lease owner).

## Alternatives considered

Keep paths but share a network filesystem: still leaks host layout and provides no tenant scoping. Store spills inline in the event stream: bloats the durable stream, which is exactly what truncation exists to avoid. Object storage first: a heavier dependency; deferred, and the interface leaves room for it.
