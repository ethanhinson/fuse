<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0128 — Loop artifact store adapter for spilled tool output and compaction segments](../../changes/active/0128-loop-artifact-store-adapter-for-spilled-tool-output-and-comp.md)**
<!-- docket:backlink:end -->

# Loop artifact store adapter for spilled tool output and compaction segments

- Type: feat
- Priority: high

## Why

Two kinds of recoverable context live on local disk behind package globals:

- Spill: `internal/tools/spill.go:31-51` holds `spillDir` process-wide; `Registry.Execute` applies `SpillOutput` to every result (`registry.go:140`), which embeds the absolute path in the `tool.result` payload (`spill.go:133-135`) that is then durable on the stream. `SetSpillDir` is called only by the CLI bindings (`cmd/fuse/main.go:187`, `shell.go:158`, `research_probe.go:78`); the loop-server bindings never set it, so hosted loops lose the elided middle with no recovery (`spill.go:136-137`).
- Segments: `agent.SegmentSink` (`internal/agent/segment.go:24`) has a noop and `fssink.FSSegmentSink` (`internal/segment/fssink/sink.go`) writing under `~/.fuse/sessions/<id>/segments`; `segment_read` reads a process-global dir (`internal/tools/segment_read.go:17-42`); the loop server passes a nil sink (`cmd/fuse/loop_server.go:439,477`), so hosted loops never archive and `segment_read` is a tool that can never succeed.

Under (a)/(b)/(c) a path in the transcript names a disk the loop no longer runs next to; under (d) nothing is in the shared store; and because `read_file` is unconfined today (0129) a spill path is a cross-tenant read.

## What changes

1. New package `internal/artifact`:
   ```go
   type Kind string // "spill", "segment", "segment-index"
   type Ref struct { Tenant event.TenantID; Loop event.LoopID; Kind Kind; ID string } // String() "art://<tenant>/<loop>/<kind>/<id>"; ParseRef
   type Meta struct { Ref Ref; Size int64; CreatedAt time.Time; Tool string; Turn int; Labels map[string]string }
   type Store interface {
       Put(ctx context.Context, ref Ref, r io.Reader, meta Meta) (Meta, error)   // ref.ID == "" ⇒ store allocates
       Open(ctx context.Context, ref Ref) (io.ReadCloser, Meta, error)         // ErrNotFound
       List(ctx context.Context, tenant event.TenantID, loop event.LoopID, kind Kind) ([]Meta, error)
   }
   ```
   Implementations: `artifact/fsstore` (`<baseDir>/<tenant>/<loop>/<kind>/<id>`, gzip at rest as today's segments, tmp+rename), `artifact/pgstore` (`//go:build pgstore`; table `artifacts(tenant_id, loop_id, kind, id, meta jsonb, body bytea, created_at)`, bodies gzip-compressed, 16 MiB cap per artifact enforced in `Put`), and `artifact.MemStore` for tests. Tenant isolation: `Open` under the wrong tenant is `ErrNotFound`.
2. Conformance suite `internal/artifact/artifacttest.RunStoreConformance(t, newStore)`: round-trip, store-allocated ids are unique under concurrency, `List` order and tenant scoping, wrong-tenant `Open` is `ErrNotFound`, gzip transparency, size cap, `Open` of a missing ref. All three impls pass.
3. Spill: `tools.Registry` gains `WithArtifacts(store artifact.Store, tenant, loop)`; `SpillOutput` becomes a method that writes `Kind:"spill"` and embeds `Full output saved as artifact art://… — read_file it or grep it with that ref`. `read_file`, `grep` and `list_directory` accept an `art://` path and resolve it through the loop's store (read-only). `SetSpillDir` is deleted; the CLI bindings construct an `artifact/fsstore` under `~/.fuse/artifacts`.
4. Segments: `internal/segment/artsink` implements `agent.SegmentSink` over `artifact.Store` (segment body as `Kind:"segment"`, the index as `Kind:"segment-index"` rewritten on each archive); `Pointer` returned is the ref string. `segment_read` takes the store + loop key instead of a dir and resolves turn ranges through the index artifact. `fssink` is kept for the shell but reimplemented as `artsink` over `artifact/fsstore` so there is one code path; `SetSegmentsDir` is deleted.
5. Wiring: `runtime.Deps.Artifacts artifact.Store` (nil ⇒ `MemStore` with a 64 MiB per-loop cap so one-shot runs still recover). `cmd/fuse/durable_backend*.go` builds `artifact/pgstore` when the event store is Postgres, `artifact/fsstore` otherwise; the loop server passes a non-nil `SegmentSink` per loop (`loop_server.go:439,477`).
6. Tests: two-runtime test — loop on A produces a >20 KiB tool result (scripted gateway asks for a fixture `bash`/`read_file`); the stream shows an `art://` ref; after a move, B's loop calls `read_file` on the ref and gets the full output. A second test: compaction on A archives a segment; B's `segment_read` for that turn range recovers it.

## Out of scope

S3/GCS backend (the interface is blob-shaped on purpose; a later change adds it); retention/purge; spill thresholds; the shell's TUI drill-in beyond resolving refs.

## Acceptance criteria

1. `artifacttest.RunStoreConformance` passes for fsstore, MemStore, and pgstore (`-tags pgstore`).
2. No `tool.result` payload produced by a hosted loop contains an absolute filesystem path for spill; a hosted >20 KiB result carries an `art://` ref and the full body is retrievable through `read_file` on another runtime sharing the store.
3. `segment_read` on a runtime that did not perform the compaction recovers the segment; `grep` on an `art://` ref works.
4. `tools.SetSpillDir` and `tools.SetSegmentsDir` no longer exist; `grep -rn "SetSpillDir\|SetSegmentsDir"` is empty.
5. Shell behaviour is preserved: segments and spills still land under `~/.fuse` and `segment_read` still works (existing 0030 adversarial parity tests pass).
6. ADR: artifacts are addressed by tenant-scoped refs, never paths; the store is an adapter with local and Postgres backends.
