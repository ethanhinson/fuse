<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0128 — Loop artifact store adapter for spilled tool output and compaction segments](../changes/archive/2026-11-04-0128-loop-artifact-store-adapter-for-spilled-tool-output-and-comp.md)**
<!-- docket:backlink:end -->

# Loop artifact store adapter for spilled tool output and compaction segments — Results

**Human action:** No action is required before merge. One Important check is worth doing on a real Postgres-backed loop server: confirm that a large tool result and a compaction segment from one replica can be read on another.

## Outcome

Two kinds of recoverable loop context used to live on local disk behind process-wide globals: the full text of oversized tool results ("spill"), and transcript segments archived before compaction. Hosted loops never configured either, so a large output was simply cut off and `segment_read` could never succeed. Turning them on would have written one replica's absolute file path into the durable transcript.

Both are now **artifacts** in one `artifact.Store`, scoped by tenant and loop and addressed by opaque refs of the form `art://<tenant>/<loop>/<kind>/<id>`:

- The store has three backends. `artifact/fsstore` is local, under `~/.fuse/artifacts`, gzip at rest. `artifact/pgstore` is a Postgres `artifacts` table, behind the `pgstore` build tag, with a 16 MiB cap per artifact. `MemStore` is the default when no store is configured, capped at 64 MiB per loop. All three pass one conformance suite, `artifacttest.RunStoreConformance`.
- Every loop's tool registry is bound to its store and its (tenant, loop) key. A truncated result now carries `Full output saved as artifact art://…` and never a path. `read_file`, `grep` and `list_directory` accept `art://` refs. A ref under another tenant or loop reads as "not found".
- The loop server picks the Postgres store when its event store is Postgres, and the filesystem store otherwise. It now gives each loop a real segment sink, so hosted loops archive segments and `segment_read` works on any replica sharing the store.
- `tools.SetSpillDir` and `tools.SetSegmentsDir` are deleted. The shell still keeps spills and segments under `~/.fuse` (now `~/.fuse/artifacts`), through `fssink.Open`.
- ADR-0080 records the decision: artifacts are addressed by tenant-scoped refs, never paths.

Departures from the spec: when a store I/O error occurs, the tools now print a fixed message naming only the ref, and the underlying error is dropped. This was a review fix, so host paths cannot leak into the transcript. `fuse orchestrate` is deliberately left without a store. It never spilled to disk before this change either.

## Human actions and testing

### Important — Cross-replica recovery on a real Postgres deployment

The automated two-runtime tests share an fsstore directory between two in-process runtimes. The pgstore backend passed its conformance suite against a real Postgres container, but no test ran a networked loop server end to end on Postgres. Without this check, a wiring mistake in that one path (`selectArtifactStore` with a DSN) would be caught only by `TestSelectArtifactStorePGWithDSN`, which skips unless `FUSE_TEST_PG_DSN` is set.

Prerequisites: a Postgres instance, and two loop-server processes built with `-tags pgstore` pointing at the same DSN. Use a cheap gateway model, never a premium provider.

1. Run `FUSE_TEST_PG_DSN=<dsn> go test -tags pgstore ./cmd/fuse -run TestSelectArtifactStorePGWithDSN -count=1`.
   Expected: PASS (not SKIP).
2. On server A, start a loop whose task makes a tool produce more than 20 KiB of output, for example `bash` running `seq 1 20000`.
   Expected: the `tool.result` event shows `Full output saved as artifact art://<tenant>/<loop>/spill/<id>` and no filesystem path.
3. Stop server A's ownership of the loop (move or resume it on server B), then ask the loop to `read_file` that ref.
   Expected: the full output comes back, including lines from the elided middle.
4. In psql, run `SELECT kind, count(*) FROM artifacts GROUP BY kind;`.
   Expected: there is at least one `spill` row.

Cleanup: `DELETE FROM artifacts WHERE loop_id = '<loop>';`

## Verification performed

- Full suite (`make test`) is green at the final head, through the build gate. The first gate run was red only on `TestLoopSpecFirstRequestGolden`, because the tool descriptions now mention `art://`. The golden was regenerated after checking that the diff was exactly those descriptions.
- Conformance suite passes for MemStore and fsstore, and for pgstore against a real Postgres container (`-tags pgstore`, Docker available, not skipped).
- Cross-runtime acceptance tests (`cmd/fuse/artifact_acceptance_test.go`): a spill ref produced on runtime A is readable through `read_file` on runtime B, and no A-side `tool.result` contains a host path. A segment archived by compaction on A is recovered by B's `segment_read`. Mutation probes showed each test turns red when its wiring is removed.
- The 0030 adversarial gzip/plaintext parity tests and the shell segment gateway test pass. `grep -rn "SetSpillDir\|SetSegmentsDir"` over `*.go` is empty.
- Whole-branch review (deep tier): 5 findings (2 important, 3 minor). Four were fixed in-branch. The fifth, the missing ADR, was resolved by recording ADR-0080. The full table is in the PR body.
- Some workers' focused test commands masked exit codes through pipes. Their pass/fail evidence came from the test output, and the full-suite gate certified the branch.

## Known issues and follow-ups

### Artifacts are never deleted

Every spill and segment is kept forever, in `~/.fuse/artifacts` locally and in the `artifacts` table on a server. The old spill directory had a 7-day sweep, and that sweep is gone. On a busy server the table grows without bound. This is confirmed, and retention was out of scope by design. Workaround: delete old rows or directories by hand. Suggested next action: a retention/purge change for the artifact store. No existing change fits (checked 29); capture a new change.

### The segment index assumes a single writer per loop

Each loop's segment index is one artifact, rewritten on every archive. If two runtimes archive for the same loop at the same moment, for example an old owner still running during a lease handoff, one index entry can be lost. The segment body is still stored, but `segment_read` cannot find it by turn range. This is a suspected edge case, not observed. Its likelihood depends on the fenced-ownership work in progress in #126. Suggested next action: once #126 lands, decide whether to derive the index from `List` instead. No existing change fits (checked 29); capture a new change if it is still needed after #126.

### Object-storage backend

The store interface is blob-shaped, but only local and Postgres backends exist, and Postgres bodies are capped at 16 MiB. Suggested next action: add an S3/GCS backend when large artifacts or workspace snapshots need it. Related to #130 (workspace snapshots over the artifact store mention object storage later). Folding a general backend into #130 would change what #130 is for, so capture a new change and link #130 under `related:`.

### pgstore Open with an oversized stored body has no direct test

The review fix caps decompression in pgstore `Open`, but no test stores an over-cap body directly. The risk is low: the code calls the shared, tested `ReadCapped` helper.
