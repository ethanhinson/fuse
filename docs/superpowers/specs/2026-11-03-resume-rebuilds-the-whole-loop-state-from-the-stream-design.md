<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0127 — Resume rebuilds the whole loop state from the stream](../../changes/active/0127-resume-rebuilds-the-whole-loop-state-from-the-stream.md)**
<!-- docket:backlink:end -->

# Resume rebuilds the whole loop state from the stream

- Type: fix
- Priority: high

## Why

`reconstructMessages` (`internal/runtime/reconstruct.go:11-77`) folds `user.input`, `model.call.end`, `tool.call`, `tool.result` and nothing else, and it has no `NodeID`/`Depth` filter even though children emit onto the same stream key (`cmd/fuse/loop_server.go:446-448`; `Resume` replays `Replay(ctx, key, 0)` at `inproc.go:1183`). Consequences on a resume after a move or an idle reap:

- Child `user.input`/`model.call.end`/`tool.*` interleave into the ROOT transcript by seq (latent bug, no test covers it).
- A child mid-flight when the owner died leaves `spawn.start` without `spawn.done` and the parent's `tool.call(spawn_agent)` without a `tool.result`; the reconstructed transcript ends in an assistant message with unanswered tool calls, which providers reject. Same for `pipeline_run`.
- The tree is rebuilt root-only (`inproc.go:549`) so `Scheduler.childCount()==0` and `SessionTokens()==0`: `agents.max_spawns`, `throughput.session_tokens`, workflow pool `total`/`tokens` quotas (`internal/agent/scheduler.go:41-87,154-217`) silently reset.
- `context.summarize` carries no summary text (`event.SummarizePayload`) and is skipped, so the summary inserted at `loop.go:524-525` is lost; the first resumed call pays the full raw history and a fresh compaction.

## What changes

1. **Payload additions** (`internal/event/event.go`): `SummarizePayload.Summary string` (the ODSNF text) and `SummarizePayload.InsertAt int` (index in the pre-compaction root transcript, or the `TurnEnd` anchor); `SpawnDonePayload` gains `Orphaned bool`; `ToolResultPayload` gains `Synthetic bool`. `loop.go:510-517` emits the summary text.
2. **Fold** (`internal/runtime/reconstruct.go` → a `loopState` fold): input is the replayed stream; output is `{messages []model.Message, tree TreeView, budgets Budgets, summaries []anchor}`.
   - Transcript: only events with `NodeID == rootID` (or `Depth == 0`); `Synthetic` tool results are folded like real ones.
   - Tree view: `spawn.start` → child node (id, parent, label, depth, role); `spawn.done` → terminal; children with `spawn.start` and no `spawn.done` are "orphans".
   - Budgets: `spawnsUsed = count(spawn.start)`; `sessionTokens = Σ model.call.end usage` across all nodes; per-pool totals via `SpawnStartPayload.Pool` (new optional field emitted by `workflow.go` when a spawn lands in a workflow pool) and per-node token sums.
   - Summaries: the latest `context.summarize` replaces the summarized region with `buildSummaryMessage(Summary, Pointer)`; tool messages inside the summarized turn range are dropped exactly as the live path does.
3. **Orphan repair on resume** (`Resume`, `inproc.go:1141-1222`): for each orphan child the resuming runtime appends `spawn.done{ChildNodeID, Err:"owner lost before completion", Orphaned:true}`; for each root `tool.call` without a `tool.result` it appends `tool.result{ID, Name, Result:"tool did not complete: the loop was resumed on another instance", IsError:true, Synthetic:true}`. These are appended BEFORE the fresh agent launches, and only after `Claim` succeeded (0126 if merged first; otherwise after the existing Heartbeat/SetLive) so a losing resume appends nothing. Pending `spawn.start` from the trailing exchange counts toward `consumedTurns` (`turnspan.go:277-298`).
4. **Seeding**: `launchLoop` seeds `tree` from the TreeView (terminal children as finished nodes so `childCount()` is right), calls `Scheduler.SeedUsage(spawns, sessionTokens, perPool)` (new method), and seeds the agent's `previousSummary` so the next compaction anchors correctly.
5. **Tests**: golden streams under `internal/runtime/testdata/streams/*.jsonl` (root-only, root+child completed, root+orphan child, pipeline orphan, summarized then resumed, summarized twice); a two-runtime test where A runs a loop that spawns a child via the scripted gateway, A is killed mid-child (cancel A's base context without `Shutdown`), the clock passes the lease, B resumes: B's seed transcript is API-valid (every assistant tool call has a tool message), contains no child turns, has `childCount()==1`, and the stream ends with `spawn.done{Orphaned:true}` then `tool.result{Synthetic:true}` then `loop.parked`.

## Out of scope

Retrying orphaned children; durable blackboard; pipeline run records; cross-replica spawning; any change to the live (non-resume) transcript path beyond carrying `Summary`.

## Acceptance criteria

1. A resumed root transcript never contains an event with `NodeID != rootID`; test pins it.
2. After resume every assistant message's tool calls have matching tool messages (synthetic where needed) and the synthetic results are durable on the stream with `Synthetic:true`.
3. `SpawnBudget().used`, `SessionTokens()` and pool totals after resume equal the values a never-moved loop would report (golden assertions).
4. A loop summarized before resume seeds with the summary message, not the raw region; `tokens` of the seed are ≤ the pre-resume estimate.
5. The two-runtime move test passes with fsstore in `go test ./...`.
6. Existing `reconstruct_test.go`, `resume_test.go`, 0117 cancel tests unchanged in outcome; `loop_serve_net_twoinstance_test.go` still passes.
7. ADR: resume is a full fold of the stream; orphans are failed durably, never silently dropped or retried.
