<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0127 — Resume rebuilds the whole loop state from the stream](../changes/active/0127-resume-rebuilds-the-whole-loop-state-from-the-stream.md)**
<!-- docket:backlink:end -->

# Results — Resume rebuilds the whole loop state from the stream (#0127)

Human action: review and merge PR #134 (https://github.com/ethanhinson/fuse/pull/134) — the resume fold, orphan repair, and budget seeding are implemented, tested, and gated green.

## Outcome

Implemented and verified: reconstructLoopState replaces reconstructMessages as a full fold of the durable event stream. Root-node filtering keeps child turns out of the root transcript. Orphan repair appends synthetic spawn.done and tool.result events durably so the seed transcript is always API-valid. Budget seeding (SeedResume + SetSeedSummary) restores spawn count, session tokens, pool tokens, and previous summary. make test green. PR #134 open.

## Summary

reconstructMessages grew into reconstructLoopState, a full fold of the durable event stream that filters by node, builds a tree view, seeds budgets, carries summaries, and repairs orphans durably.

## What was done

- Payload additions (additive, omitempty): SummarizePayload.Summary/InsertAt, SpawnDonePayload.Orphaned, ToolResultPayload.Synthetic, SpawnStartPayload.Pool.
- Full fold (reconstructLoopState): root-node filtering, tree view from spawn.start/done, budgets, summaries.
- Orphan repair on resume: synthetic spawn.done and tool.result appended durably before the fresh agent launches.
- Seeding: AgentTree.SeedResume populates node count + token totals; Agent.SetSeedSummary seeds previousSummary.
- Summary emission: live path emits Summary text and InsertAt in context.summarize.

## Tests

Golden stream tests, TestReconstructLoopStateGolden, TestRepairOrphansMakesTheTranscriptAPIValid, TestRepairOrphansCleanStreamAppendsNothing, TestRepairOrphansIsIdempotent, TestResumeRepairsOrphansOnTheStream, TestSeedResume, TestPayloadAdditionsRoundTrip, TestPayloadAdditionsAbsentAreZero. Existing reconstruct/resume/cancel tests unchanged in outcome.

## Build evidence

make test green, go build clean, go vet clean.

## ADR

ADR-0081: Resume is a full fold of the stream; orphans are failed durably, never silently dropped or retried.

## PR

https://github.com/ethanhinson/fuse/pull/134
