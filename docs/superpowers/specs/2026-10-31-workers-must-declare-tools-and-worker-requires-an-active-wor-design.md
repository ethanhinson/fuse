<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0095 — Workers must declare tools, and worker requires an active workflow](../../changes/active/0095-workers-must-declare-tools-and-worker-requires-an-active-wor.md)**
<!-- docket:backlink:end -->

# Workers must declare tools, and worker requires an active workflow

Designed against main after the per-spawn workflow activation change (PR #115) merged. Line numbers are approximate for that state.

## Problem

Three gaps, all silent.

1. A worker with no tools inherits everything. `WorkerConfig.Tools` is a plain slice (`internal/config/schema.go` around 519-525) and the comment above it, like the proto's (`proto/fuse/harness/v1/harness.proto` around 600-603, "Omitting spawn_agent makes the worker unable to nest"), promises that an allowlist without `spawn_agent` cannot nest. The runtime does not keep that promise for an empty allowlist: `resolveWorkerTools` returns `w.Tools` as-is for a known worker with no requested subset (`cmd/fuse/workflow.go` around 93-95), `childToolRegistry` treats an empty list as "clone the parent" (`cmd/fuse/run.go` around 528-531), and `shouldWireChildSpawn` treats it as "wire spawn_agent" (`run.go` around 548-550). So `workers: {reader: {model: house}}` yields a child with every tool and the spawn tool, inside a pool that was meant to fence it. Neither validator catches it: `Config.Problems` has no workflow rule (`internal/config/validate.go` around 76-165), the harness validator reaches only those rules through `SemanticProblems` (`internal/harnessconfig/semantic.go` around 56-65), and the proto declares only `items.string.min_len = 1` on `tools`, not a minimum count. A hosted spec with a tool-less worker is accepted at `loop.start` (`internal/harnessresolve/resolve.go` around 60-95). The loader also replaces a workflow's `workers` map wholesale on override (`internal/config/loader.go` around 195), so an operator who narrows the built-in `research` workflow to `workers: {facet-researcher: {model: x}}` gets an unfenced worker without knowing it.

2. A worker outside any workflow is ignored. `checkRequest` returns nil for a worker name when the parent has no governing activation (`workflow.go` around 336-342, "outside every workflow a worker is ignored (as before)"), and `prepareChild` returns the requested subset with the worker dropped (`workflow.go` around 362-365). Pipeline steps reach the same spot: `spawnOnce` sets `Worker: s.Worker` (`internal/pipeline/engine.go` around 349-353) and `runPipelineSteps` with no workflow runs the steps on the caller's spawner (`run.go` around 728-731), so a definition whose steps name `worker:` runs each step with the parent's full tool set and no error. The synthesis palette offers every worker of every configured workflow regardless of context (`run.go` around 858-867), so a synthesized plan outside a workflow can name a worker that then means nothing.

3. `pool.tokens` is unproven in a host. The chain exists: a hosted child's call gate is `sched.TokenQuotaDenied(childNode.ID)` (`cmd/fuse/loop_server.go` around 419), which refuses once `SubtreeTokens(poolID) >= pool.Tokens` (`internal/agent/scheduler.go` around 209-214); the agent ends a non-interactive loop with that error before calling the model (`internal/agent/loop.go` around 458-465); usage reaches the node from the gateway reply's `usage` object (`internal/model/adapter.go` around 498-500, `loop.go` around 624, `internal/agent/tree.go` around 159). But no hosted test drives it: every scripted reply in the hosted workflow tests carries no `usage` (`cmd/fuse/workflow_activation_test.go` around 83-110), so the pool's token quota could stop being enforced in a host without a test going red.

## Design

### 1. A worker declares at least one tool

- `Config.Problems` (`internal/config/validate.go`) gains a rule, after the existing ones, iterating workflows and workers in sorted name order: a worker whose `Tools` is empty adds `Problem{Path: "workflows.<wf>.workers.<worker>.tools", Message: "must name at least one tool (a worker inherits nothing; omit spawn_agent to keep it from nesting)"}`. The CLI startup gate (`cmd/fuse/main.go` around 60) and the harness validator both read `Problems`, so one rule covers both.
- `SpecPath` (`internal/harnessconfig/harnessconfig.go` around 748-768) learns to walk map fields: when a field is a map, the next path part is its key and is rendered as `["<key>"]` with `strconv.Quote`, and the walk continues into the map's value message. `workflows.wf.workers.reader.tools` therefore maps to `workflows["wf"].workers["reader"].tools`, the same shape `tightenField` already produces for pool fields (`internal/harnessresolve/resolve.go` around 219-226) and the same shape protovalidate reports. `tightenField`'s hand-rolled branch is replaced by the shared walk.
- The proto adds `(buf.validate.field).repeated.min_items = 1` on `Worker.tools`, so the shape layer, which every language evaluates, reports the same field with rule id `repeated.min_items`; `Combine` keeps the shape violation and drops the duplicate config problem for that path (`semantic.go` around 72-90). Regenerate the Go and TS types.
- Comments corrected in both places: a worker's allowlist is its whole tool set; it must name at least one tool, inherits nothing, always receives the five blackboard tools in addition, and is unable to nest exactly when the allowlist omits `spawn_agent`. The spawn tool's `worker` description (`internal/tools/spawn_agent.go` around 208) says the same.
- Defense in depth: `resolveWorkerTools` returns `fmt.Errorf("worker %q declares no tools", worker)` for a known worker with an empty allowlist, so a config that bypassed validation can never hand a worker the parent's registry.
- No inherit-all flag is added; a worker that wants every tool lists them.

### 2. A worker needs an active workflow

- Add `agent.ErrWorkerOutsideWorkflow = errors.New("agent: worker requires an active workflow")` next to `ErrNestedWorkflow` (`internal/agent/spawn.go` around 33-35).
- `checkRequest` (`workflow.go` around 336-342): when `worker != ""`, `workflow == ""` and `activationOf(parent)` is nil, return `fmt.Errorf("%w: worker %q named outside any workflow; pass workflow (available: %s)", agent.ErrWorkerOutsideWorkflow, worker, <sorted workflow names or "none configured">)`. This fires before any node exists or quota is spent, like the unknown-name refusals.
- `prepareChild` (`workflow.go` around 362-365): the `act == nil` branch returns the same error when `opts.Worker != ""`, covering paths that call `Spawner.Spawn` directly.
- `makePipelineRunFn` (`run.go` around 678-704): after `resolvePipelineWorkflow`, when `workflow == ""`, the node is outside any workflow subtree and any step has `Worker != ""`, refuse before yielding the slot: `step %q names worker %q but no workflow is active; pass workflow or set the definition's workflow field`. `loopWiring.wirePipeline` passes `w.scope.activationOf` (as `inWorkflow func(nodeID string) bool`) into `makePipelineRunFn` and `makePipelineSynthFn` for this check. Inside a workflow subtree, steps keep resolving against the governing workflow as today.
- `makePipelineSynthFn` (`run.go` around 760-830): the palette's workers are those of the `workflow` argument when given, else those of the governing workflow when the node is inside a subtree, else none. `pipelineSynthPalette` keeps returning the union for callers that still want it, but the synth seam no longer offers a worker it cannot bind. The same up-front step check runs on the synthesized DAG before `runPipelineSteps`.
- The error text names the remedy, as the record asks: "pass workflow".

### 3. `pool.tokens` proven in a host

No runtime change. A hosted acceptance test (below) drives the whole chain through the production loop-server factory with a spec-defined `pool.tokens`, using gateway replies that carry `usage`.

### ADR

Record one ADR, status Accepted, superseding the per-spawn activation ADR (ADR-0067), which is marked Superseded. It restates that ADR's decisions 1 through 4 unchanged (per-spawn `workflow` argument and root accounting; no nesting; the pipeline coordinator; blackboard tools for worker-bound children) and replaces decision 5 with:

- Unknown workflow and worker names are refused before any node is created (unchanged).
- A `worker` named outside any active workflow is refused, by `spawn_agent` and by pipeline steps alike, with an error that says to pass `workflow`. It is no longer ignored.
- Every worker declares at least one tool. The config validator, the harness validator and the shape rules all refuse an empty allowlist at the same field path; the runtime refuses it again. A worker's allowlist is its whole tool set, so "omit `spawn_agent` to prevent nesting" is true.
- Alternatives: an `inherit_all: true` worker flag (rejected: the record scopes it out, and an inheriting worker is not a worker); keeping "ignored" for a worker outside a workflow and only logging (rejected: the author cannot see a log from a hosted loop, and the tool result is the one channel the model and the author share); validating only in the harness validator (rejected: the CLI and the hosted path must agree, which `Problems` guarantees).

## Testing

`internal/config`:

- `TestProblems_WorkerWithoutTools`: a config with `workflows: {wf: {workers: {reader: {model: house}}}}` yields one problem with `Path == "workflows.wf.workers.reader.tools"`; two tool-less workers in two workflows yield two problems in sorted order; the built-in defaults yield none; an override `workflows: {research: {workers: {facet-researcher: {model: x}}}}` over the defaults yields the problem for `workflows.research.workers.facet-researcher.tools` (the wholesale `workers` replacement is what makes this reachable).

`internal/harnessconfig` and `harness`:

- `TestSpecPath_WalksMaps`: `SpecPath("workflows.wf.workers.reader.tools") == `workflows["wf"].workers["reader"].tools``; `SpecPath("workflows.wf.pool.total") == `workflows["wf"].pool.total``; a key with a quote is escaped; `models.default` still maps to `models.default_alias`.
- `TestValidate_WorkerWithoutTools`: `harness.Validate` on a spec with `Workflows: {"wf": {Workers: {"reader": {}}}}` returns exactly one violation, `FieldPath == `workflows["wf"].workers["reader"].tools``, `RuleID == "repeated.min_items"`; `ValidateShape` returns the same one. A spec whose worker has `Tools: ["read_file"]` returns none.
- `internal/harnessresolve`: `TestResolve_RefusesWorkerWithoutTools`: `Resolver.Resolve` returns a `*harness.ValidationError` carrying that violation, so `loop.start` refuses the spec.

`cmd/fuse`:

- `TestResolveWorkerTools_EmptyAllowlistErrors`: a known worker with `Tools: nil` returns an error containing `declares no tools`, with and without a requested subset.
- `TestCheckRequest_WorkerOutsideWorkflowRefused`: `checkRequest(parent, "", "reader")` with the parent outside any workflow returns an error wrapping `agent.ErrWorkerOutsideWorkflow` whose text contains `pass workflow` and the configured workflow names; with `workflow: "wf"` it passes; inside a `wf` subtree it passes.
- `TestPrepareChildRefusesWorkerOutsideWorkflow` replaces `TestPrepareChildIgnoresWorkerOutsideWorkflow` (`cmd/fuse/workflow_scope_test.go` around 208): the plan is an error wrapping `agent.ErrWorkerOutsideWorkflow`.
- `TestPipelineRunFnRefusesWorkerStepsOutsideWorkflow`: a definition with a `worker: reader` step, no workflow argument, no definition workflow, node outside any workflow: the tool errors naming the step and `pass workflow`, no spawn happens (the scripted spawner records zero calls), and the slot was never yielded. With `workflow: wf` the same definition runs under the coordinator (existing `TestPipelineRunFnWorkflowUsesRunner` stays green).
- `TestPipelineSynthPaletteOutsideWorkflowOffersNoWorkers`: the synth context's `Workers` is empty outside a workflow with no argument; equals `wf`'s workers with `workflow: wf`; equals the governing workflow's workers inside its subtree.
- `TestHostedWorkflowRefusals` (`cmd/fuse/loop_server_workflow_test.go` around 267) gains a root-level `spawn_agent {worker: "reader"}` with no `workflow`: the root's turn-1 tool result contains `worker "reader" named outside any workflow; pass workflow`, no node is created for it, and the gateway receives no request for its task.
- `TestHostedWorkflowTokenPoolRefusesWorkerModelCall` (end-to-end, production loop-server wiring, the record's named test). Spec: `Workflows: {"wf": {Pool: {Total: 3, Tokens: 100}, Workers: {"reader": {Tools: ["read_file"]}}}}`. Add a `toolCallsReplyUsage(in, out int, calls ...call)` and `textReplyUsage(s string, in, out int)` beside the existing helpers that set `"usage": {"prompt_tokens": in, "completion_tokens": out}` on the reply (the non-streaming parse reads it, `adapter.go` around 498-500). Route: `ROOT-TASK` turn 0 spawns `lead` with `workflow: wf`; later root turns reply text. `LEAD-TASK` turn 0 spawns one `worker: reader` child `r0` with task `READ-TASK`; later lead turns reply text. `READ-TASK` turn 0 replies `toolCallsReplyUsage(80, 40, call{blackboard_write, {key: "k", value: "v"}})`; any later `READ-TASK` turn replies text. Run through `runHosted`-style wiring over `harnessServerWith`, `StartLoop`, `Wait`, `Attach(..., 0)`. Assert:
  - `gw.callsFor("READ-TASK")` has exactly one request: the reader's second model call (the one that would answer the tool result) was refused by the call gate before reaching the gateway, because the subtree spend (120) now exceeds `pool.tokens` (100).
  - `gw.callsFor("LEAD-TASK")` has exactly one request: the lead's turn-1 call is refused too, since the lead is the workflow root and `SubtreeTokens(lead)` counts the reader's spend (`internal/agent/subtree.go` around 100-112).
  - The root's turn-1 tool result for `lead` contains `spawn_agent:` and `workflow 120/100 tokens used`.
  - The durable history holds an `error` event for the reader's node and one for the lead's node whose text contains `tokens used`.
  - `tree.SubtreeTokens(lead.ID) == 120`, `tree.Scheduler().Pool(lead.ID).Tokens == 100`, and `nodeLabelled(t, tree, "r0")` reports the reader completed with an error, not a result.
  - The root's own loop finishes normally: `pool.tokens` is a subtree quota and must not touch the root outside it (the root's turn-1 request reaches the gateway).

## Compatibility

- Config and spec tightening, for the release note: a workflow worker with no `tools` is refused at CLI startup, by `harness.Validate`, and at `loop.start`, with the field path `workflows["<wf>"].workers["<worker>"].tools`. Operators who relied on an empty allowlist meaning "everything" list the tools they want. Overrides of the built-in `research` workflow that restate `workers` must restate `facet-researcher`'s tools.
- `spawn_agent` and pipeline steps that named a `worker` outside any workflow used to run with the full tool set; they are now refused with an error that says to pass `workflow`. A model that did this will see the error and self-correct; a hand-written pipeline definition needs a `workflow:` field or the tool argument.
- Synthesized pipelines outside a workflow no longer see worker names in their palette.
- The proto gains a validation rule; the wire shape is unchanged. The TS shape validator reports the new rule with the same path.
- `agent.ErrWorkerOutsideWorkflow` is a new exported sentinel.

## Dependencies and order

Builds on the merged per-spawn activation change (PR #115) and nothing else. Independent of the provenance change (0094) and the unparseable-command change (0096); it may be built before, between or after them. Build it in its own PR: its release note is about config validation and spawn semantics, theirs about permissions.
