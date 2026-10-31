<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0095 — Workers must declare tools, and worker requires an active workflow](../../changes/active/0095-workers-must-declare-tools-and-worker-requires-an-active-wor.md)**
<!-- docket:backlink:end -->

# Workers must declare tools, and worker requires an active workflow - Implementation Plan

> Executed by `docket-build`: one task at a time, each task TDD (failing test, minimal code, passing test, one commit). Every test uses scripted fakes only: stub child builders, the wiring and scope harnesses in `cmd/fuse`, or a scripted `httptest` gateway. No live model traffic of any kind.

**Goal:** Close three silent gaps. (1) A workflow worker that declares no tools is refused by the config validator, the harness validator (shape and semantic layers, same field path) and `loop.start`, and the runtime refuses it again. (2) A `worker` named by `spawn_agent` or by a pipeline step with no active workflow is refused with an error that says to pass `workflow`, instead of being ignored and running with the full tool set. (3) A hosted end-to-end test proves `pool.tokens` refuses a worker's model call once the workflow's token pool is spent.

**Architecture:**
- `internal/config/validate.go`: one new rule in `Config.Problems`, so the CLI startup gate and `harnessconfig.SemanticProblems` agree.
- `internal/harnessconfig/harnessconfig.go`: `SpecPath` walks map fields (`workflows.wf.workers.reader.tools` becomes `workflows["wf"].workers["reader"].tools`); `harnessresolve.tightenField` drops its hand-rolled workflow branch and uses the shared walk.
- `proto/fuse/harness/v1/harness.proto`: `repeated.min_items = 1` on `Worker.tools`; stubs regenerated through `make proto` (never hand-edited).
- `internal/agent/spawn.go`: new sentinel `ErrWorkerOutsideWorkflow`.
- `cmd/fuse/workflow.go`: `resolveWorkerTools` refuses an empty allowlist; `checkRequest` and `prepareChild` refuse a worker outside every workflow; new `workflowScope.governing` lookup.
- `cmd/fuse/run.go` and `cmd/fuse/loop_wiring.go`: the pipeline run and synth seams take a `workflowLookup`; they refuse worker-naming steps that no workflow will govern, and the synth palette offers only workers it can bind.

**Tech stack:** Go, protovalidate (`buf.build/bufbuild/protovalidate`), buf codegen (Go + TS), `cmd/fuse` test harnesses, `httptest` scripted gateway.

**Writing rules for everything this plan produces (code comments, error strings, test names, commit messages):** no em dashes (use a colon, a comma, parentheses or a hyphen). Existing em dashes in untouched lines stay as they are. Do not reference any product or application outside this repository.

**Learnings applied:**
- `patch-every-cloned-child-builder`: after change 0091 every binding's child wiring goes through `loopWiring.childTools` and `loopWiring.wirePipeline`. Before Task 5, re-derive the call sites by grep (`grep -n 'prepareChild\|resolveWorkerTools\|makePipelineRunFn(\|makePipelineSynthFn(' cmd/fuse/*.go | grep -v _test`), and do not trust this plan's list. If a second production site appears, it gets the same change.
- `security-knob-inert-at-composition-root`: every refusal here is proven through a composition root as well as in the package: the CLI gate reads `Problems` (Task 1), `loop.start` goes through `Resolver.Resolve` (Task 3), and the hosted tests drive the production loop-server factory (Task 6).
- `scripted-gateway-double-double-escapes-tool-args`: the new reply helpers build tool-call arguments with `json.Marshal` of a Go map (reuse `argsJSON`) and embed them once. Never hand-write pre-escaped JSON.
- `shared-worktree-red-suite-verify-detached`: before trusting a red or green full-suite verdict, check `git status --porcelain`; if the tree is not clean, re-run at the branch tip in a detached worktree.

**Decisions made in this plan (record them in the PR body):**
1. The pipeline refusal wraps `agent.ErrWorkerOutsideWorkflow` as well, so both refusal surfaces (`spawn_agent`, pipeline steps) are matchable with `errors.Is`.
2. The pipeline seams take `governing workflowLookup` (`func(nodeID string) (config.WorkflowConfig, bool)`), backed by a new `workflowScope.governing`. A nil lookup means "no workflow scope": every node is treated as outside a workflow.
3. `makePipelineSynthFn` drops its `workers []string` parameter: the palette is computed per call (the `workflow` argument's workers, else the governing workflow's workers, else none). `pipelineSynthPalette` keeps its signature; `wirePipeline` ignores its `workers` result.
4. The synthesized-DAG worker check runs after synthesis (the plan is already published to `pipeline.plan` and traced) but before any step spawns; it un-yields the caller's slot before returning the error.
5. Existing behavior kept: a synthesized plan inside a workflow subtree that names a workflow is still refused as nested by `admitPipeline`. This change does not alter that path.

**ADR:** recorded by the parent through `docket-adr` (`adr.supersede`), not by a build worker. See Task 7.

---

## Task 1: Config - a worker must name at least one tool

**Tier:** economy

**Files:**
- Modify: `internal/config/validate.go`
- Modify: `internal/config/schema.go` (the `WorkerConfig` comment)
- Modify: `internal/config/problems_test.go`

**Step 1: Write the failing test** `TestProblems_WorkerWithoutTools` in `internal/config/problems_test.go`:
- `cfg := Default(); cfg.Workflows["wf"] = WorkflowConfig{Workers: map[string]WorkerConfig{"reader": {Model: "house"}}}`. `ps := cfg.Problems()`; exactly one problem; `ps[0].Path == "workflows.wf.workers.reader.tools"`; `ps[0].Message == "must name at least one tool (a worker inherits nothing; omit spawn_agent to keep it from nesting)"`; `cfg.Validate().Error() == "config: workflows.wf.workers.reader.tools must name at least one tool (a worker inherits nothing; omit spawn_agent to keep it from nesting)"`.
- Two tool-less workers in two workflows (`b` with worker `z`, `a` with worker `y`, plus a tooled worker `x` in `a` that must not be reported) yield exactly two problems in sorted order: `workflows.a.workers.y.tools`, then `workflows.b.workers.z.tools`.
- `Default().Problems()` is empty (the built-in `research` workflow's `facet-researcher` has tools). `TestProblemsDefaultIsEmpty` already covers this; keep it green.
- Override over the defaults: `c, err := FromYAML([]byte("workflows:\n  research:\n    workers:\n      facet-researcher: {model: x}\n"), "test")`, `err == nil`, and `c.Problems()` contains a problem with `Path == "workflows.research.workers.facet-researcher.tools"` (the loader's wholesale `workers` replacement in `mergeWorkflows` is what makes this reachable).
- A worker with `Tools: []string{}` (empty, non-nil) is reported the same as nil.

**Step 2: Run, confirm red:** `go test ./internal/config -run 'TestProblems_WorkerWithoutTools' -count=1`.

**Step 3: Implement** in `Config.Problems`, after the observability check (the last existing rule), so `Problems()[0]` for every existing config is unchanged:
```go
// A workflow worker's allowlist is its whole tool set: it inherits nothing
// from its parent, so an empty allowlist is a mistake, not "everything".
for _, wf := range sortedKeys(c.Workflows) {
	workers := c.Workflows[wf].Workers
	for _, w := range sortedKeys(workers) {
		if len(workers[w].Tools) == 0 {
			add("workflows."+wf+".workers."+w+".tools", "must name at least one tool (a worker inherits nothing; omit spawn_agent to keep it from nesting)")
		}
	}
}
```
Use `sort.Strings` over collected keys (a small local generic helper `sortedKeys[V any](m map[string]V) []string` in `validate.go` is fine; do not import from `cmd/fuse`). The message contains no `%`, so passing it to `add` as the format string is safe.

Update the `WorkerConfig` comment in `schema.go`:
```go
// WorkerConfig is a typed worker: a tool allowlist and an optional model pin
// (empty: inherit the parent's model). The allowlist is the worker's whole tool
// set: it must name at least one tool and inherits nothing from the parent. A
// worker-bound child also always receives the five blackboard tools, and it can
// nest exactly when its allowlist names spawn_agent.
```

**Step 4: Green:** `go test ./internal/config -count=1`.

**Step 5: Commit** `fix(config): refuse a workflow worker that declares no tools`

---

## Task 2: `SpecPath` walks map fields

**Tier:** standard

**Files:**
- Modify: `internal/harnessconfig/harnessconfig.go` (`SpecPath`)
- Modify: `internal/harnessresolve/resolve.go` (`tightenField`)
- Modify: `internal/harnessconfig/harnessconfig_test.go`

**Step 1: Write the failing test** `TestSpecPath_WalksMaps` in `internal/harnessconfig/harnessconfig_test.go`:
- `SpecPath("workflows.wf.workers.reader.tools") == `workflows["wf"].workers["reader"].tools``
- `SpecPath("workflows.wf.pool.total") == `workflows["wf"].pool.total``
- `SpecPath("workflows.research.workers.facet-researcher.model") == `workflows["research"].workers["facet-researcher"].model``
- `SpecPath("models.entries.fast.id") == `models.entries["fast"].id``
- A key containing a quote is escaped with `strconv.Quote`: `SpecPath(`workflows.a"b.pool.total`) == `workflows["a\"b"].pool.total``
- `SpecPath("workflows.wf") == `workflows["wf"]``
- `SpecPath("workflows.wf.nope") == ""` (unknown field below a map value) and `SpecPath("workflows") == "workflows"`.
- A map whose value is not a message cannot be walked past its key: pick such a field by reading the descriptor (if `HarnessSpec` has none, skip this case with a comment).
- `models.default` still maps to `models.default_alias`, and every case in the existing `TestSpecPath` keeps its expected value.

**Step 2: Run, confirm red:** `go test ./internal/harnessconfig -run 'TestSpecPath' -count=1`.

**Step 3: Implement.** Rewrite the loop in `SpecPath` to build the output with a `strings.Builder`:
- For each part: look up the field in the current message descriptor; `nil` returns `""`. Append `.` (except first) and the field name.
- If the field is a map: if it is the last part, return. Otherwise the next part is the key: append `"[" + strconv.Quote(key) + "]"` and advance past it. If the key is the last part, return. Otherwise the map value must be a message (`fd.MapValue().Message() != nil`), else return `""`; continue with that message.
- If the field is a list: it must be the last part (as today), else return `""`.
- If it is a singular message field and not last, continue into `fd.Message()`; a scalar that is not last returns `""`.
Keep the doc comment accurate: mention that a map key renders as `["<key>"]`, the shape protovalidate reports.

In `harnessresolve.tightenField`, delete the `workflows.` branch and return `harnessconfig.SpecPath(key), limit`. Workflow names cannot contain dots (the proto key pattern), so the shared walk yields the same `workflows["<name>"].pool.<field>` text.

**Step 4: Green:** `go test ./internal/harnessconfig ./internal/harnessresolve ./harness -count=1`. `TestResolveReportsEveryViolationWithPaths` (expects `workflows["research"].pool.concurrent`) and the ceilings tests must stay green unchanged.

**Step 5: Commit** `fix(harnessconfig): map config paths through map fields to spec field paths`

---

## Task 3: Proto rule, regenerated stubs, and the validator and resolver tests

**Tier:** standard

**Files:**
- Modify: `proto/fuse/harness/v1/harness.proto` (`Worker.tools` rule and comment)
- Regenerate (never hand-edit): `harness/v1/harness.pb.go`, `proto/gen/ts/fuse/harness/v1/harness_pb.ts`
- Modify: `harness/testdata/shape_cases.json`
- Modify: `harness/validate_test.go`
- Modify: `internal/harnessresolve/resolve_test.go`

**Step 1: Write the failing tests.**
- `harness/testdata/shape_cases.json`: append a case `{"name": "worker without tools", "spec": {"workflows": {"wf": {"workers": {"reader": {}, "ok": {"tools": ["read_file"]}}}}}, "want": [{"fieldPath": "workflows[\"wf\"].workers[\"reader\"].tools", "ruleId": "repeated.min_items"}]}`. This one file drives both `TestValidateShapeSharedCases` (Go) and the TS `validateHarnessShape` test.
- `harness/validate_test.go` `TestValidate_WorkerWithoutTools`: `harness.Validate(&harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{"wf": {Workers: map[string]*harnessv1.Worker{"reader": {}}}}})` returns exactly one violation with `FieldPath == `workflows["wf"].workers["reader"].tools`` and `RuleID == "repeated.min_items"` (the semantic duplicate for the same path is dropped by `Combine`). `harness.ValidateShape` on the same spec returns the same single violation. A spec whose worker has `Tools: []string{"read_file"}` returns no violations from either.
- `internal/harnessresolve/resolve_test.go` `TestResolve_RefusesWorkerWithoutTools`: `Resolver{Base: operator(t), Skills: installed("research")}.Resolve(ctx, "acme", spec)` with the same spec returns an error; `violations(t, err)` contains exactly one violation, with that field path and rule id. Add a second sub-case that restates the operator-defined `research` workflow's workers without tools (`"research": {Workers: {"facet-researcher": {}}}`): one violation at `workflows["research"].workers["facet-researcher"].tools`.

**Step 2: Run, confirm red:** `go test ./harness ./internal/harnessresolve -run 'WorkerWithoutTools|SharedCases' -count=1`. Expect the shape cases to fail (no `repeated.min_items` yet).

**Step 3: Implement.**
- In `harness.proto`, `message Worker`, field `tools`, replace the rule and the comment:
  ```proto
  // Tool allowlist: the worker's whole tool set. It must name at least one tool
  // and inherits nothing from the parent. A worker-bound child always receives
  // the five blackboard tools in addition, and it can nest exactly when the
  // allowlist names spawn_agent.
  repeated string tools = 1 [
    (buf.validate.field).repeated = {
      min_items: 1
      items: {string: {min_len: 1}}
    },
    (fuse.harness.v1.ui) = {
      label: "Tools"
      widget: WIDGET_LIST
    }
  ];
  ```
  Also update the `message Worker` comment to say the allowlist is the whole tool set.
- Regenerate with the repo's generate target, from the feature worktree root:
  1. Install the TS plugin once if absent: `test -x node_modules/.bin/protoc-gen-es || npm ci` (root npm workspace; `node_modules/` is git-ignored).
  2. `PATH="$HOME/go/bin:$PATH" make proto` (runs `proto/generate.sh`: `buf lint` then `buf generate`). The pinned toolchain is in `$HOME/go/bin` (`protoc-gen-go v1.36.12`, `protoc-gen-connect-go 1.20.0`, `buf`). `buf` may fetch the protovalidate dependency from the Buf Schema Registry.
  3. `git status --porcelain` must show only `proto/fuse/harness/v1/harness.proto`, `harness/v1/harness.pb.go` and `proto/gen/ts/fuse/harness/v1/harness_pb.ts` (plus the test files). If `npm ci` changed `package-lock.json`, restore it with `git checkout -- package-lock.json`; do not commit lockfile churn.
  4. If codegen cannot run (tool missing, registry unreachable), stop and return BLOCKED with the exact command and output. Never hand-edit generated files.

**Step 4: Green:**
- `go test ./harness/... ./internal/harnessconfig ./internal/harnessresolve -count=1`
- Drift guard with the toolchain on PATH, so it runs instead of skipping: `PATH="$HOME/go/bin:$PATH" go test ./harness/v1 -run TestGeneratedStubsHaveNoDrift -count=1 -v` (confirm it reports PASS, not SKIP; if it skips on the TS plugin, note that in the task report).
- TS lane: `make sdk-ts-test` (needs node; it reads `harness/testdata/shape_cases.json`). Restore any `package-lock.json` change it makes before committing.

**Step 5: Commit** `fix(harness): refuse a workflow worker with an empty tool allowlist in the spec`

---

## Task 4: Runtime - empty allowlist and worker outside a workflow refused

**Tier:** standard

**Files:**
- Modify: `internal/agent/spawn.go` (sentinel)
- Modify: `cmd/fuse/workflow.go` (`resolveWorkerTools`, `checkRequest`, `prepareChild`, comments)
- Modify: `internal/tools/spawn_agent.go` (the `worker` property description)
- Modify: `cmd/fuse/workflow_test.go`
- Modify: `cmd/fuse/workflow_scope_test.go`

**Step 1: Write the failing tests.**
- `cmd/fuse/workflow_test.go` `TestResolveWorkerTools_EmptyAllowlistErrors`: a workflow with `Workers: {"reader": {}}`. `resolveWorkerTools(wf, "reader", nil)` and `resolveWorkerTools(wf, "reader", []string{"read_file"})` both return a nil slice and an error whose text contains `worker "reader" declares no tools`. The existing `resolveWorkerTools` tests stay green.
- `cmd/fuse/workflow_scope_test.go` `TestCheckRequest_WorkerOutsideWorkflowRefused`, on `newScopeHarness(t, scopeWF())`:
  - `h.scope.checkRequest(h.root, "", "reader")` returns an error with `errors.Is(err, agent.ErrWorkerOutsideWorkflow)`, whose text contains `worker "reader" named outside any workflow; pass workflow (available: wf)`.
  - `h.scope.checkRequest(h.root, "wf", "reader")` returns nil.
  - Inside a subtree: `lead := h.lead(); h.scope.checkRequest(lead, "", "reader")` returns nil.
  - A scope with no workflows (`newScopeHarness(t, nil)`): the text contains `(available: none configured)`.
  - Through the spawn func: `h.scope.spawnFunc(h.spawner(h.root), h.root)(ctx, tools.SpawnRequest{Label: "x", Task: "t", Worker: "reader"})` returns that error and the tree's node count is unchanged.
- Replace `TestPrepareChildIgnoresWorkerOutsideWorkflow` with `TestPrepareChildRefusesWorkerOutsideWorkflow`: `p := h.spawn(h.root, agent.SpawnOpts{Label: "x", Task: "t", Worker: "reader", Tools: []string{"bash"}})`; `errors.Is(p.err, agent.ErrWorkerOutsideWorkflow)` and the text contains `pass workflow`. (`h.spawn` goes through `Spawner.Spawn` directly, so this covers the pipeline-step path.)

**Step 2: Run, confirm red:** `go test ./cmd/fuse -run 'TestResolveWorkerTools|TestCheckRequest_WorkerOutside|TestPrepareChild' -count=1` (compile failure on `agent.ErrWorkerOutsideWorkflow` is the expected red).

**Step 3: Implement.**
- `internal/agent/spawn.go`, after `ErrNestedWorkflow`:
  ```go
  // ErrWorkerOutsideWorkflow refuses a worker named where no workflow is active
  // (change 0095): a worker resolves only against a workflow, so outside one the
  // name has no meaning and is refused rather than ignored.
  var ErrWorkerOutsideWorkflow = errors.New("agent: worker requires an active workflow")
  ```
- `cmd/fuse/workflow.go`:
  - `resolveWorkerTools`: after the unknown-worker check, `if len(w.Tools) == 0 { return nil, fmt.Errorf("worker %q declares no tools", worker) }`. Update the rules comment: a known worker's allowlist is its whole tool set, and an empty one is refused (defense in depth behind the validators).
  - Add a helper beside `unknownWorkflowErr`:
    ```go
    // workerOutsideWorkflowErr is the one refusal for a worker named where no
    // workflow is active. names is the sorted list of configured workflows.
    func workerOutsideWorkflowErr(worker string, names []string) error {
    	list := "none configured"
    	if len(names) > 0 {
    		list = strings.Join(names, ", ")
    	}
    	return fmt.Errorf("%w: worker %q named outside any workflow; pass workflow (available: %s)", agent.ErrWorkerOutsideWorkflow, worker, list)
    }
    ```
  - `checkRequest`: in the `workflow == ""` branch, when `worker != ""` and `activationOf(parent.ID)` is nil, return `workerOutsideWorkflowErr(worker, s.names())`. Update the doc comment ("outside every workflow a worker is refused").
  - `prepareChild`: in the `act == nil` branch, `if opts.Worker != "" { return childPlan{}, workerOutsideWorkflowErr(opts.Worker, s.names()) }`. Update its doc comment.
  - `childPlan.tools` comment: "nil = inherit all" stays true only for freeform spawns; say so.
- `internal/tools/spawn_agent.go`, the `worker` property description: `"Workflow worker type for the child. Its allowlist is the child's whole tool set (plus the blackboard tools); a worker whose allowlist omits spawn_agent cannot itself spawn."`. If a `spawn_agent` test pins the old description text, update it in the same commit.

**Step 4: Green:** `go test ./internal/agent ./internal/tools ./cmd/fuse -count=1`. Investigate any other `cmd/fuse` test that spawned a `worker` from outside a workflow (the grep at planning time found none); fix the test only if it relied on the old "ignored" behavior, and say so in the commit body.

**Step 5: Commit** `fix(fuse): refuse a worker named outside any workflow, and an empty worker allowlist`

---

## Task 5: Pipelines - refuse unbound worker steps, narrow the synth palette

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/workflow.go` (`workflowScope.governing`, `workflowLookup` type)
- Modify: `cmd/fuse/run.go` (`makePipelineRunFn`, `makePipelineSynthFn`, new `refuseUnboundWorkerSteps`)
- Modify: `cmd/fuse/loop_wiring.go` (`wirePipeline`)
- Modify: `cmd/fuse/loop_wiring_test.go`
- Modify: `cmd/fuse/pipeline_gateway_e2e_test.go` (call-site signature only)

**Step 0:** re-derive the production call sites (learning `patch-every-cloned-child-builder`): `grep -n 'makePipelineRunFn(\|makePipelineSynthFn(' cmd/fuse/*.go | grep -v _test`. At planning time the only production caller is `loopWiring.wirePipeline`.

**Step 1: Write the failing tests** in `cmd/fuse/loop_wiring_test.go`.
- `TestPipelineRunFnRefusesWorkerStepsOutsideWorkflow`: `h := newWiringHarness(t)`; `runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root, h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing)`. Record `before := len(h.tree.SnapshotAll())` and `slots := h.tree.Scheduler().Snapshot().SlotsInUse`. `_, err := runFn(ctx, []byte(wiringPipelineDef), "")`: `errors.Is(err, agent.ErrWorkerOutsideWorkflow)`, text contains `step "s1" names worker "reader"` and `pass workflow`. No node was created (`len(h.tree.SnapshotAll()) == before`), the builder saw no spawn (`len(h.seen) == 0`), and `h.tree.Scheduler().Snapshot().SlotsInUse == slots` (the refusal runs before `YieldSlot`; the ordering in the diff is the primary evidence). With `workflow: "wf"` the same definition completes (`pipeline p: completed`). With the definition's own `workflow: wf` field and no argument, it completes too. Inside a subtree (`lead := h.lead()`, run fn built at `lead.node`), the worker definition with no workflow runs and the step resolves `reader` against the governing workflow. A nil lookup (`governing == nil`) at the root refuses like outside.
- Update `TestPipelineRunFnWorkflowUsesRunner`: its "plain" sub-case ran `wiringPipelineDef` (a `worker: reader` step) with no workflow and expected completion. That is now refused by design. Give the plain sub-case a worker-less definition (`const wiringPlainPipelineDef = "name: p\nsteps:\n  - name: s1\n    prompt: hi\n"`) and keep its assertions (completed, zero coordinator nodes). The `wf` and nil-runner sub-cases are unchanged apart from the new argument.
- `TestPipelineSynthPaletteOutsideWorkflowOffersNoWorkers`: `h := newWiringHarness(t)`; before any spawn add a second workflow to the shared map: `h.cfg.Workflows["other"] = config.WorkflowConfig{Workers: map[string]config.WorkerConfig{"writer": {Tools: []string{"write_file"}}}}` (`h.w.cfg` and the scope share this map). Capture the synthesis child's `opts.SystemPrompt` in `h.reply` (label `synthesize`) and reply `{"name":"p","workflow":"wf","steps":[{"name":"s1","prompt":"hi"}]}`.
  - Root, no argument: the captured prompt does not contain `Available worker types:`.
  - Root, `workflow: "wf"`: it contains `Available worker types: reader\n` and not `writer`.
  - Inside the `wf` subtree (`lead := h.lead()`, synth fn built at `lead.node`, no argument): it contains `Available worker types: reader\n` and not `writer`. Ignore that call's result (a synthesized plan naming `wf` inside the subtree is refused as nested, which is unchanged behavior).
- `TestPipelineSynthFnRefusesUnboundWorkerSteps`: a synth fn with a nil runner and nil lookup at the root (a context with no workflow scope), whose synthesis reply is `{"name":"p","workflow":"wf","steps":[{"name":"s1","worker":"reader","prompt":"hi"}]}`: the call returns an error wrapping `agent.ErrWorkerOutsideWorkflow` and naming step `s1`; no step child spawned (only the `synthesize` node exists); a second call on the same harness still works (the slot was un-yielded; the call must not hang under `context.WithTimeout(..., 5*time.Second)`).

**Step 2: Run, confirm red:** `go test ./cmd/fuse -run 'TestPipelineRunFn|TestPipelineSynth' -count=1` (compile failure on the new arguments and `governing` is the expected red).

**Step 3: Implement.**
- `cmd/fuse/workflow.go`:
  ```go
  // workflowLookup reports the workflow governing nodeID, if any. A nil lookup
  // means the context has no workflow scope: every node is outside a workflow.
  type workflowLookup func(nodeID string) (config.WorkflowConfig, bool)

  // governing is the scope's workflowLookup.
  func (s *workflowScope) governing(nodeID string) (config.WorkflowConfig, bool) {
  	act, _ := s.activationOf(nodeID)
  	if act == nil {
  		return config.WorkflowConfig{}, false
  	}
  	return act.cfg, true
  }
  ```
- `cmd/fuse/run.go`:
  ```go
  // refuseUnboundWorkerSteps refuses a pipeline whose steps name a worker that
  // no workflow will govern: no workflow argument or definition field, and node
  // outside every workflow subtree. It runs before any step spawns.
  func refuseUnboundWorkerSteps(p *pipeline.Pipeline, workflow string, node *agent.AgentNode, governing workflowLookup) error {
  	if workflow != "" {
  		return nil
  	}
  	if governing != nil {
  		if _, ok := governing(node.ID); ok {
  			return nil
  		}
  	}
  	for _, s := range p.Steps {
  		if s.Worker != "" {
  			return fmt.Errorf("%w: step %q names worker %q but no workflow is active; pass workflow or set the definition's workflow field", agent.ErrWorkerOutsideWorkflow, s.Name, s.Worker)
  		}
  	}
  	return nil
  }
  ```
  - `makePipelineRunFn(spawner, bb, sched, node, runWF workflowRunner, governing workflowLookup)`: call `refuseUnboundWorkerSteps(p, workflow, node, governing)` after `resolvePipelineWorkflow` and the nil-runner check, before `sched.YieldSlot(node)`. Update the doc comment.
  - `makePipelineSynthFn(spawner, bb, sched, node, cfg, toolNames []string, traceW, runWF workflowRunner, governing workflowLookup)` (drop `workers`): compute `palette` per call: the argument's workflow workers when `workflow != ""` (existing branch), else `sortedWorkerNames(wf)` when `governing != nil` and `governing(node.ID)` returns ok, else nil. After `if workflow == "" && runWF != nil { workflow = p.Workflow }`, call `refuseUnboundWorkerSteps(p, workflow, node, governing)`; on error un-yield (`if !sched.UnyieldSlot(ctx, node) { return "", ctx.Err() }`) and return it. Update the doc comment (palette and refusal).
- `cmd/fuse/loop_wiring.go` `wirePipeline`: `_, toolNames := pipelineSynthPalette(w.cfg, reg)`; pass `w.scope.governing` to both seams. Update the `pipelineSynthPalette` doc comment to say the shared wiring uses only its tool names now.
- Update every test call site for the new signatures: `loop_wiring_test.go` (lines around 358, 372, 382, 403, 435, 456: pass `h.w.scope.governing`, or `nil` where the test passed a nil runner, and drop the `workers` argument) and `pipeline_gateway_e2e_test.go` (lines around 139, 140, 212, 298, 299: pass `nil` for the lookup and drop the `workers` argument). `TestPipelineSynthFnWorkflowUsesRunner` keeps asserting `Available worker types: reader\n` with `workflow: wf`.

**Step 4: Green:** `go test ./cmd/fuse ./internal/pipeline -count=1`, then `go test ./cmd/fuse -race -count=1 -run 'Pipeline|Runner|Wiring'`.

**Step 5: Commit** `fix(fuse): refuse pipeline worker steps with no active workflow, and narrow the synthesis palette`

---

## Task 6: Hosted acceptance - outside-workflow refusal and `pool.tokens`

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/workflow_activation_test.go` (reply helpers with usage)
- Modify: `cmd/fuse/loop_server_workflow_test.go`

**Step 1: Helpers (test-only).**
- In `workflow_activation_test.go`, beside `toolCallsReply` and `textReply`, add `toolCallsReplyUsage(in, out int, calls ...call) string` and `textReplyUsage(s string, in, out int) string`: the same bodies plus a top-level `"usage": {"prompt_tokens": in, "completion_tokens": out}` (the non-streaming parse in `internal/model/adapter.go` reads `Usage.PromptTokens` and `Usage.CompletionTokens`). Build them through `json.Marshal` of Go maps; factor the shared body so `toolCallsReply` and `textReply` are the zero-usage case without changing their output.
- In `loop_server_workflow_test.go`, split `runHosted` into `runHostedSpec(t, spec *harnessv1.HarnessSpec, route)` and keep `runHosted(t, route)` as `runHostedSpec(t, wfSpec(), route)`. Return the runtime and the loop handle too (or add a sibling that does) so a test can call `rt.Attach(context.Background(), "", h.ID(), 0)` for the durable history; keep existing callers compiling.

**Step 2: Write the failing tests.**
- Extend `TestHostedWorkflowRefusals`: the root's turn-0 reply gains a third call, `call{name: "spawn_agent", args: map[string]any{"label": "w", "task": "W-TASK", "worker": "reader"}}` (no `workflow`). Assert the root's turn-1 tool results contain exactly one result with `worker "reader" named outside any workflow; pass workflow`; `gw.callsFor("W-TASK")` is empty; no node is labelled `w` (add `"w"` to the existing refused-label switch and `"W-TASK"` to the marker loop).
- `TestHostedWorkflowTokenPoolRefusesWorkerModelCall` (the record's named end-to-end test, production loop-server wiring). Spec: `&harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{"wf": {Pool: &harnessv1.Pool{Total: 3, Tokens: 100}, Workers: map[string]*harnessv1.Worker{"reader": {Tools: []string{"read_file"}}}}}}`. Route:
  - `ROOT-TASK` turn 0: `toolCallsReply(call{spawn_agent, {label: "lead", task: "LEAD-TASK", workflow: "wf"}})`; later root turns: `textReply("root done")`.
  - `LEAD-TASK` turn 0: `toolCallsReply(call{spawn_agent, {label: "r0", task: "READ-TASK", worker: "reader"}})`; later lead turns: `textReply("lead done")`.
  - `READ-TASK` turn 0: `toolCallsReplyUsage(80, 40, call{name: "blackboard_write", args: map[string]any{"key": "k", "value": "v"}})`; later reader turns: `textReply("read done")`.
  Run through `runHostedSpec`. Assert:
  1. `len(gw.callsFor("READ-TASK")) == 1`: the reader's second model call was refused by the call gate before reaching the gateway, because the subtree spend (120) is at or over `pool.tokens` (100).
  2. `len(gw.callsFor("LEAD-TASK")) == 1`: the lead's turn-1 call is refused too. The lead is the workflow root, its call gate resolves `WorkflowRootOf(lead) == lead`, and `SubtreeTokens(lead)` counts the reader's spend (it excludes only the lead's own tokens).
  3. The root's turn-1 request reaches the gateway (`turnRequest(t, gw, "ROOT-TASK", 1)` exists): `pool.tokens` is a subtree quota and does not touch the root outside it. Its tool result for `lead` contains `workflow 120/100 tokens used`, and it is rendered as a spawn error (assert the error prefix the spawn tool actually renders for a failed child; at planning time this is expected to start with `spawn_agent:`, so read `internal/tools/spawn_agent.go` and assert the real prefix).
  4. The durable history from `Attach(..., 0)` holds an `event.KindError` event whose `NodeID` is the reader's node id and one whose `NodeID` is the lead's node id, each with a payload containing `tokens used`.
  5. `tree.SubtreeTokens(lead.ID) == 120`; `pool, ok := tree.Scheduler().Pool(lead.ID)` with `ok && pool.Tokens == 100`; `nodeLabelled(t, tree, "r0").Status == agent.StatusError`.
  If an assertion fails because the chain behaves differently from this description (for example the lead's gate is not reached), do not weaken the assertion: report NEEDS_ESCALATION with the observed trace, since the test exists to catch exactly that.

**Step 3: Run:** `go test ./cmd/fuse -run 'TestHostedWorkflow' -count=1 -v`. The refusal case is red until Task 4 is in (it is, by order). The token test exercises existing runtime code; if it passes on first run, that is expected (the change adds the proof, not the mechanism). Confirm it is a real proof by temporarily setting the reader usage to `(10, 10)` locally and seeing assertion 1 fail, then revert. Do not commit the temporary edit.

**Step 4: Green:** `go test ./cmd/fuse -count=1` and `go test ./cmd/fuse -race -count=1 -run 'TestHosted'`.

**Step 5: Commit** `test(fuse): hosted loops refuse an unbound worker and enforce a workflow token pool`

---

## Task 7: ADR superseding ADR-0067 (parent task, not a build worker)

**Tier:** parent (run by `docket-implement-next` through the `docket-adr` agent, operation `adr.supersede`; a build worker skips this task)

Record one ADR, status Accepted, superseding ADR-0067 ("Workflows activate per spawn, do not nest, and root at a coordinator"), which becomes Superseded. Suggested title: "Workers declare their whole tool set and require an active workflow". Content:
- Restates ADR-0067 decisions 1 through 4 unchanged: the per-spawn `workflow` argument and root accounting; workflows do not nest; the pipeline coordinator node; the blackboard tools for worker-bound children.
- Replaces decision 5 with:
  - Unknown workflow and worker names are refused before any node is created (unchanged).
  - A `worker` named outside any active workflow is refused, by `spawn_agent` and by pipeline steps alike, with an error that says to pass `workflow` (`agent.ErrWorkerOutsideWorkflow`). It is no longer ignored. The synthesis palette offers only workers it can bind.
  - Every worker declares at least one tool. The config validator, the harness validator and the shape rules (`repeated.min_items`) refuse an empty allowlist at the same field path, `workflows["<wf>"].workers["<worker>"].tools`; the runtime refuses it again. A worker's allowlist is its whole tool set, so "omit `spawn_agent` to prevent nesting" holds.
- Alternatives considered: an `inherit_all: true` worker flag (rejected: out of the change's scope, and an inheriting worker is not a worker); keeping "ignored" for a worker outside a workflow and only logging (rejected: the author cannot see a log from a hosted loop; the tool result is the one channel the model and the author share); validating only in the harness validator (rejected: the CLI and the hosted path must agree, which `Config.Problems` guarantees).
- Link change 0095. Record it before the PR is opened so the PR body can reference the new ADR id.

---

## Task 8: Final verification

**Tier:** economy

**Steps:**
1. `gofmt -l ./cmd ./internal ./harness` prints nothing.
2. `go build ./... && go vet ./...`
3. `make test` (runs `observability-validate` then `go test ./...`), then `go test ./... -count=1 -race`.
4. `PATH="$HOME/go/bin:$PATH" go test ./harness/v1 ./internal/loopwire/... -run Drift -count=1 -v`: the drift guards pass (not skip) where the toolchain is present.
5. `make sdk-ts-test`; restore any `package-lock.json` churn.
6. Stale text sweep: `grep -rn 'worker is ignored\|any worker is ignored\|Omitting spawn_agent makes the worker' cmd internal proto harness` returns nothing outside generated files that were regenerated from the updated proto.
7. `git diff origin/main --stat` touches no generated file except the two regenerated stubs, and `git log` shows one commit per task.
8. If anything needed a fix, commit `chore(fuse): post-verification fixes for worker validation`. Otherwise do not create a commit.

## PR body notes (for the parent)

Release-note items, from the spec's Compatibility section:
- A workflow worker with no `tools` is refused at CLI startup, by `harness.Validate`, and at `loop.start`, at field path `workflows["<wf>"].workers["<worker>"].tools`. Operators who relied on an empty allowlist meaning "everything" list the tools they want. Overrides of the built-in `research` workflow that restate `workers` must restate `facet-researcher`'s tools.
- `spawn_agent` calls and pipeline steps that named a `worker` outside any workflow used to run with the full tool set; they are now refused with an error that says to pass `workflow`. A hand-written pipeline definition needs a `workflow:` field or the tool argument.
- Synthesized pipelines outside a workflow no longer see worker names in their palette.
- The proto gains a validation rule; the wire shape is unchanged. The TS shape validator reports the new rule at the same path.
- `agent.ErrWorkerOutsideWorkflow` is a new exported sentinel.

## Acceptance coverage map

| Record / spec item | Test |
|---|---|
| Config validation refuses a tool-less worker with a field path | Task 1 `TestProblems_WorkerWithoutTools` |
| Harness validation refuses it at the same path (shape and semantic, one violation) | Task 3 `TestValidate_WorkerWithoutTools`, shared `shape_cases.json` (Go and TS) |
| `loop.start` refuses the spec | Task 3 `TestResolve_RefusesWorkerWithoutTools` |
| Spec paths walk maps; tighten paths unchanged | Task 2 `TestSpecPath_WalksMaps`, existing resolver and ceilings tests |
| Runtime refuses an empty allowlist | Task 4 `TestResolveWorkerTools_EmptyAllowlistErrors` |
| Schema and proto comments corrected | Tasks 1, 3, 4 |
| `spawn_agent` with `worker` and no active workflow refused, says "pass workflow" | Task 4 `TestCheckRequest_WorkerOutsideWorkflowRefused`, `TestPrepareChildRefusesWorkerOutsideWorkflow`; Task 6 `TestHostedWorkflowRefusals` |
| Pipeline step with `worker` and no active workflow refused | Task 5 `TestPipelineRunFnRefusesWorkerStepsOutsideWorkflow`, `TestPipelineSynthFnRefusesUnboundWorkerSteps` |
| Synth palette offers only bindable workers | Task 5 `TestPipelineSynthPaletteOutsideWorkflowOffersNoWorkers` |
| Hosted `pool.tokens` refuses a worker's model call once spent | Task 6 `TestHostedWorkflowTokenPoolRefusesWorkerModelCall` |
| ADR superseding ADR-0067 | Task 7 (parent) |
