<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0132 — Pipeline runs are event-sourced and resumable](../../changes/active/0132-pipeline-runs-are-event-sourced-and-resumable.md)**
<!-- docket:backlink:end -->

# Pipeline runs are event-sourced and resumable

- Type: feat
- Priority: medium

## Why

`pipeline.Run` (`internal/pipeline/engine.go:73`) keeps `done/skipped/released/routerDone/started` (`:104-108`), retries (`:320`) and a `WaitGroup` in locals; step inputs/outputs are blackboard keys; the only durable trace is the children's `spawn.*` events. On a move the parent's `tool.call(pipeline_run)` dangles (0127 fails it synthetically) and the work is lost.

## What changes

1. Events: `pipeline.start{RunID, Name, Steps []StepRef, Digest}`, `pipeline.step.start{RunID, Step, Attempt, Inputs []string}`, `pipeline.step.done{RunID, Step, Attempt, Status: done|skipped|failed, Output key, Err}`, `pipeline.route{RunID, Step, Chosen}`, `pipeline.done{RunID, Status, Err}`; emitted from the engine through the loop's store (the engine receives an `event.EventStore` + node identity like the Spawner does).
2. `internal/pipeline.RunState` + `FoldRunState(events []event.Event, runID) RunState`; `type PipelineRunStore interface { Load(ctx, loop StreamKey, runID string) (RunState, error) }` with a stream-backed impl (replay + fold) and a mem impl for tests.
3. Engine refactor: `Run(ctx, def, deps)` where `deps` carries `Spawner`, the blackboard interface from 0131, the event sink, and an optional `Resume RunState`; on resume, steps with `done` are skipped, steps with a `start` but no `done` are re-driven from attempt+1 (inputs re-read from the blackboard, which 0131 restored), routes already chosen are honoured.
4. Resume policy (`internal/runtime`): when 0127's fold finds a root `tool.call(pipeline_run)` without a result and a `pipeline.start` without `pipeline.done` for it, the runtime, if `Deps.ResumePipelines` is true (default true for the server bindings), re-invokes the tool with `Resume` state instead of appending a synthetic failure; otherwise 0127's synthetic result applies and `pipeline.done{Status:failed, Err:"owner lost"}` is appended.
5. Tests: golden-stream fold tests; two-runtime test — a three-step pipeline where step 2 is in flight when A is cut; B resumes and the stream shows step 1 not re-run, step 2 attempt 2, step 3 run, `pipeline.done{done}`, and the parent's `tool.result` carries the pipeline's final output.

## Out of scope

Distributed step execution; schema changes; side-effect idempotency of re-driven steps (documented).

## Acceptance criteria

1. Every pipeline run by a hosted loop emits the lifecycle events; `FoldRunState` reproduces the engine's final maps on golden streams.
2. The two-runtime resume test passes: completed steps are not re-run, the in-flight step is re-driven once, the run completes.
3. With `ResumePipelines=false` the dangling call is failed synthetically and `pipeline.done{failed}` is on the stream.
4. Existing pipeline tests (`internal/pipeline`, `cmd/fuse` workflow tests) pass unchanged in outcome.
5. ADR: pipelines are resumable by replaying their lifecycle events; side effects of a re-driven step are not deduplicated.
