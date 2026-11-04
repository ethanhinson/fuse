<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0127 — Resume rebuilds the whole loop state from the stream](../changes/active/0127-resume-rebuilds-the-whole-loop-state-from-the-stream.md)**
<!-- docket:backlink:end -->

# Results — Resume rebuilds the whole loop state from the stream (#0127)

## Summary

`reconstructMessages` grew into `reconstructLoopState`, a full fold of the durable event stream that filters by node, builds a tree view, seeds budgets, carries summaries, and repairs orphans durably — so a loop that moves between replicas is identical to one that never moved.

## What was done

- **Payload additions** (additive, `omitempty`): `SummarizePayload.Summary`/`InsertAt`, `SpawnDonePayload.Orphaned`, `ToolResultPayload.Synthetic`, `SpawnStartPayload.Pool`.
- **Full fold** (`reconstructLoopState`): root-node filtering, tree view from spawn.start/done, budgets (spawns, session tokens, pool tokens), summaries.
- **Orphan repair on resume**: synthetic `spawn.done{Orphaned:true}` and `tool.result{Synthetic:true,IsError:true}` appended durably before the fresh agent launches.
- **Seeding**: `AgentTree.SeedResume` populates node count + token totals; `Agent.SetSeedSummary` seeds `previousSummary`.
- **Summary emission**: live path emits `Summary` text and `InsertAt` in `context.summarize`.

## Tests

- Golden stream tests (4 `.jsonl` files under `internal/runtime/testdata/streams/`)
- `TestReconstructLoopStateGolden` — verifies fold, filtering, tree, budgets, summaries
- `TestRepairOrphansMakesTheTranscriptAPIValid` — orphan repair via real fsstore
- `TestRepairOrphansCleanStreamAppendsNothing` — no-op on clean stream
- `TestRepairOrphansIsIdempotent` — second repair adds nothing
- `TestResumeRepairsOrphansOnTheStream` — full Resume path with fsstore
- `TestSeedResume` — tree seeding correctness
- `TestPayloadAdditionsRoundTrip` / `TestPayloadAdditionsAbsentAreZero` — payload backward compat
- Existing `reconstruct_test.go`, `resume_test.go`, cancel tests — unchanged in outcome

## Build evidence

- `make test` — green
- `go build ./...` — clean
- `go vet ./...` — clean

## ADR

ADR-0081: Resume is a full fold of the stream; orphans are failed durably, never silently dropped or retried.

## PR

https://github.com/ethanhinson/fuse/pull/134
