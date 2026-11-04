<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0127 — Resume rebuilds the whole loop state from the stream](../../changes/active/0127-resume-rebuilds-the-whole-loop-state-from-the-stream.md)**
<!-- docket:backlink:end -->

# Plan — Resume rebuilds the whole loop state from the stream (0127)

## Context

The feature worktree is at `.worktrees/resume-rebuilds-the-whole-loop-state-from-the-stream`,
branch `fix/resume-rebuilds-the-whole-loop-state-from-the-stream`, base `origin/main` (f01f122).
Visibility: shared.

The spec is at `docs/superpowers/specs/2026-11-03-resume-rebuilds-the-whole-loop-state-from-the-stream-design.md`.

## Task 1 — Payload additions (internal/event/event.go)

Add fields to three payload structs. All additions are additive with `omitempty` so old streams replay unchanged.

1. **SummarizePayload**: add `Summary string` (`json:"summary,omitempty"`) and `InsertAt int` (`json:"insert_at,omitempty"`).
2. **SpawnDonePayload**: add `Orphaned bool` (`json:"orphaned,omitempty"`).
3. **ToolResultPayload**: add `Synthetic bool` (`json:"synthetic,omitempty"`).
4. **SpawnStartPayload**: add `Pool string` (`json:"pool,omitempty"`).

**File**: `internal/event/event.go` (structs near lines 357–384).

**Tests**: add table-driven marshal/unmarshal cases in `internal/event/event_test.go` (or create `internal/event/payload_additions_test.go`) verifying a round-trip preserves each new field and that an old stream with the fields absent still unmarshals (zero values, no error).

**Verify**: `go test ./internal/event/...`

---

## Task 2 — Emit summary text on the live path (internal/agent/loop.go)

The live summarization path at ~line 498–508 emits a `context.summarize` event with `SummarizePayload`. Currently it carries no `Summary` text or `InsertAt`. Update the emission to include:

- `Summary`: the ODSNF summary text (the `summary` local variable from `a.summarizer.summarize`).
- `InsertAt`: the `insertAt` returned by `summarizationRegion` (the index in the root transcript where the summary message was inserted).

Read the summarization block (`internal/agent/loop.go` ~lines 485–520) to find the exact emission site and the variable names (`summary`, `insertAt`, `region`, `tokensBefore`). The payload is already constructed there — add the two fields to it.

**Tests**: add a test in `internal/agent/loop_test.go` (or a focused new test) that triggers summarization with a stub summarizer, captures the emitted `context.summarize` event, and asserts `Summary` and `InsertAt` are non-zero. If capturing events in a unit test is hard, verify via the existing summarization test harness — grep for `summarizer` / `summarize` in the test files to find the seam.

**Verify**: `go test ./internal/agent/...`

---

## Task 3 — Grow reconstructMessages into a full loopState fold (internal/runtime/reconstruct.go)

Replace `reconstructMessages(events) → []model.Message` with `reconstructLoopState(events) → loopState` where:

```go
type loopState struct {
    messages []model.Message
    tree     treeView
    budgets   budgets
    summaries []summaryAnchor
}
```

### 3a — Root-node filtering for the transcript

The `Event` struct already carries `NodeID` and `Depth`. The root node has `NodeID == rootID` (passed as a parameter) or `Depth == 0`. Filter the transcript fold to only root events: in the switch, skip any event where `e.NodeID != "" && e.NodeID != rootID` (the root's own events may have empty NodeID in older streams, so accept empty too). Synthetic tool results (`ToolResultPayload.Synthetic == true`) fold like real ones.

**Signature change**: `reconstructLoopState(events []event.Event, rootID string) (loopState, error)`. Update the call site in `Resume` (inproc.go ~line 1200) to pass `loopID` as rootID.

### 3b — Tree view from spawn.start / spawn.done

Walk events for `KindSpawnStart` and `KindSpawnDone` to build a `treeView`:

```go
type treeView struct {
    nodes []treeNode // one per spawn.start
}
type treeNode struct {
    id, parent, label, role string
    depth                   int
    terminal                bool // has spawn.done
    orphaned                bool // spawn.start without spawn.done
}
```

`spawn.start` → append a treeNode (ChildNodeID, ParentID from event, Label, Depth, Role). `spawn.done` → mark the matching node terminal. After the walk, any node without a `spawn.done` is `orphaned: true`.

### 3c — Budgets from the stream

```go
type budgets struct {
    spawnsUsed    int
    sessionTokens int
    poolTokens    map[string]int // pool ID → total tokens
}
```

- `spawnsUsed = count(spawn.start)`.
- `sessionTokens = Σ ModelCallEndPayload.InputTokens + OutputTokens` across ALL events (all nodes — session tokens are whole-tree).
- `poolTokens`: when `SpawnStartPayload.Pool` is non-empty, accumulate that child's tokens there. (Requires tracking per-node token sums — fold `ModelCallEnd` by `NodeID`.)

### 3d — Summaries

```go
type summaryAnchor struct {
    summary  string
    insertAt int
}
```

On `KindSummarize`, if `SummarizePayload.Summary` is non-empty, record a `summaryAnchor`. The latest summarize replaces earlier ones (the live path drops prior summaries via `dropPriorSummary`).

**Tests**: golden stream files under `internal/runtime/testdata/streams/*.jsonl`:
- `root_only.jsonl` — root-only, no children, no orphans.
- `root_child_completed.jsonl` — root + one completed child.
- `root_orphan_child.jsonl` — root + one orphan child (spawn.start, no spawn.done).
- `summarized_then_resumed.jsonl` — root with one context.summarize carrying Summary text.

Golden test: `internal/runtime/reconstruct_golden_test.go` — for each `.jsonl`, call `reconstructLoopState`, assert the transcript contains only root events, the tree view matches, budgets match hand-computed values, and summaries carry the text.

**Verify**: `go test ./internal/runtime/...`

---

## Task 4 — Orphan repair on resume (internal/runtime/inproc.go)

In `Resume` (inproc.go ~line 1190–1225), after `reconstructLoopState` succeeds and BEFORE `launchLoop`:

1. For each orphan child in `treeView` (has `spawn.start`, no `spawn.done`): append a `spawn.done` event to the durable stream with `SpawnDonePayload{ChildNodeID, Err: "owner lost before completion", Orphaned: true}`.
2. For each root `tool.call` without a matching `tool.result`: append a `tool.result` event with `ToolResultPayload{ID, Name, Result: "tool did not complete: the loop was resumed on another instance", IsError: true, Synthetic: true}`.

These appends go to the durable store via `r.deps.DurableStore.Append` (or whatever the append seam is — grep for `.Append(` in inproc.go). They must happen only after the resume claim succeeded (the existing lease re-ownership path above this code in `Resume`).

3. Re-read the events after appending to get the final stream for `reconstructLoopState` (or fold the synthetic events into the state directly — simpler to re-replay).

4. Update `consumedTurns` (turnspan.go) to count a pending `spawn.start` from the trailing exchange toward consumed turns (if not already). Read the existing `consumedTurns` function — it already counts trailing events after the last park, so a trailing `spawn.start` is already a trailing event. Verify this and add a test if needed.

**Tests**: the two-runtime move test (Task 6) covers this end-to-end. Add a focused unit test `internal/runtime/resume_orphan_test.go` that replays a hand-built stream with an orphan child and an unanswered root tool call, calls Resume (or the repair function directly), and asserts the durable stream now ends with `spawn.done{Orphaned:true}` then `tool.result{Synthetic:true}`.

**Verify**: `go test ./internal/runtime/...`

---

## Task 5 — Seed launchLoop from loopState (internal/runtime/inproc.go + internal/agent)

In `launchLoop`, when `opts.resume`:

1. **Tree seeding**: seed `tree` from the `treeView`. Terminal children become finished nodes; orphaned children also become finished nodes (they were failed). This makes `tree.childCount()` correct. Add a method `tree.SeedNodes(nodes []seedNode)` or build it inline — read `agent.NewAgentTreeWithRootID` and the tree's node-append path to find the seam.

2. **Budget seeding**: call `Scheduler.SeedUsage(spawnsUsed, sessionTokens, poolTokens)` — a new method on `*Scheduler`:
```go
func (s *Scheduler) SeedUsage(spawns, sessionTokens int, poolTokens map[string]int) {
    // set maxSpawns ceiling unchanged; seed the tree's append-only node count and
    // per-node token counters so SpawnBudget().used and SessionTokens() report
    // correctly after resume.
}
```
This needs the tree to support seeding node count and token counters — read `internal/agent/tree.go` for the node append path and `internal/agent/subtree.go` for token accounting. The tree may need a `SeedTokens(nodeID, in, out)` method or the scheduler may hold the seed directly.

3. **Summary seeding**: set the agent's `previousSummary` to the latest `summaryAnchor.Summary` so the next compaction anchors correctly. Find where `previousSummary` is initialized (loop.go ~line 404) and thread the summary from `launchOpts` into the agent.

**Tests**: unit test in `internal/agent/scheduler_test.go` (or `tree_test.go`) verifying `SeedUsage` makes `SpawnBudget()` and `SessionTokens()` return the seeded values.

**Verify**: `go test ./internal/agent/... && go test ./internal/runtime/...`

---

## Task 6 — Two-runtime move test (cmd/fuse/loop_serve_net_twoinstance_test.go)

Add a test that:
1. Runtime A starts a loop via the scripted gateway, spawns a child.
2. A is killed mid-child (cancel A's base context without `Shutdown`).
3. The clock passes the lease expiry.
4. Runtime B resumes: B's seed transcript is API-valid (every assistant tool call has a tool message), contains no child turns, has `childCount()==1`, and the stream ends with `spawn.done{Orphaned:true}` then `tool.result{Synthetic:true}` then `loop.parked`.

Use the existing `loop_serve_net_twoinstance_test.go` harness — read it to find the test setup pattern (fsstore, two inproc runtimes, scripted gateway). Use `fsstore` as the durable store.

**Verify**: `go test ./cmd/fuse/... -run TwoInstance -count=1`

---

## Task 7 — Update existing tests + full suite

1. Update `reconstruct_test.go` — change `reconstructMessages` calls to `reconstructLoopState` and assert `.messages` from the result.
2. Update `resume_test.go` — same, plus verify root-only filtering.
3. Verify `loop_serve_net_resume_test.go` still passes.
4. Run `make test` (the full gate: `observability-validate + go test ./...`).

**Verify**: `make test`

---

## Task 8 — ADR

Record an ADR: "resume is a full fold of the stream; orphans are failed durably, never silently dropped or retried." Use the `docket-adr` skill / `adr.record` operation. The ADR captures the non-obvious decision that orphaned children are marked done-with-error rather than retried, and that the transcript fold filters by node to keep child turns out of the root.

---

## Ordering

Tasks 1–2 are independent and can run in parallel. Task 3 depends on Task 1 (payload fields). Task 4 depends on Task 3. Task 5 depends on Tasks 3–4. Task 6 depends on Tasks 4–5. Task 7 is last. Task 8 runs after the build is green.

Critical path: 1 → 3 → 4 → 5 → 6 → 7.
