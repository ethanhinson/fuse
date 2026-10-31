<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0091 — Workflows activate per spawn in every host, enforcing pools and worker tools](../../changes/active/0091-workflows-activate-per-spawn-in-every-host-enforcing-pools-a.md)**
<!-- docket:backlink:end -->

# Per-spawn workflow activation in every host - Implementation Plan

> Executed by `docket-build`: one task at a time, each task TDD (failing test, minimal code, passing test, commit). Every test is a scripted fake (stub child builders or a scripted `httptest` gateway). No live model traffic.

**Goal:** `spawn_agent` and `pipeline_run` take an optional `workflow` argument. The spawned child (or a pipeline coordinator node) becomes that workflow's root: its pool is registered with the loop's scheduler, its subtree resolves `worker` against that workflow, worker tool allowlists restrict child schemas, and nested activation is refused. One shared path serves every binding (one-shot, shell, research probe, loop-server and loop-serve-net through the same loop factory).

**Architecture:**
- `internal/agent`: `SpawnOpts.Workflow` marks the new node's `WorkflowRoot` at creation (the field stays create-once, lock-free). `Spawner.Spawn` refuses a `Workflow` request from inside a workflow subtree (`ErrNestedWorkflow`) before any node exists. `(*Spawner).WithBuilder` returns a copy with a different child builder (used for the pipeline coordinator).
- `internal/tools`: `spawn_agent` and `pipeline_run` parse and forward `workflow`, and advertise the available workflow names (enum and description) when given them.
- `cmd/fuse/workflow.go`: a per-loop `workflowScope` owns activations keyed by workflow-root node id. It validates requests, activates (registers the pool, records the activation with its atomic `reserved` counter), resolves the governing activation for any node via `tree.WorkflowRootOf`, and supplies the backstop, budget, scoped spawn tool and per-child worker plan. It also runs a pipeline inside a fresh workflow-root coordinator node.
- `cmd/fuse/loop_wiring.go`: `loopWiring` holds the child and root tool wiring all four child-builder closures share (registry subset, scoped `spawn_agent`, blackboard, `pipeline_run`). The binding closures keep only what is truly binding-specific (renderer, approvals, handles, session log, event sink).

**Tech stack:** Go, `internal/agent`, `internal/tools`, `internal/pipeline`, `cmd/fuse`. Tests use `httptest` gateways and stub `agent.ChildBuilder`s.

**Learnings applied:**
- `patch-every-cloned-child-builder`: there are FOUR child-builder closures today (`runtime_binding.go` one-shot, research probe, shell; `loop_server.go` factory). Re-derive the list with `grep -n 'childBuilder := func' cmd/fuse/*.go` before Task 6. Do not trust this plan's count.
- `security-knob-inert-at-composition-root`: the pool and allowlist are only real if every composition root wires them. Task 6 adds a source-scan guard, and Tasks 7 and 8 drive the real deps builders end to end.
- `verify-tool-loop-at-gateway-seam`: assert the `tools[]` each child request carries at the gateway, not a hand-built registry.
- `scripted-gateway-double-double-escapes-tool-args`: build tool-call `arguments` with `json.Marshal` of a Go map, then embed that string once. Never hand-write pre-escaped JSON.
- `slot-cap-yield-while-blocked-on-children`: the pipeline coordinator runs no model. It must yield its scheduler slot while its steps run.

**Decisions made in this plan (record them in the PR body; the ADR is handled outside this plan):**
1. A workflow-root child is admitted into its PARENT's pool (the spawner's node scope) and does not count against its own `pool.total`. Only its descendants count (`SubtreeSpawnCount` already excludes the root).
2. `pipeline_run` with `workflow` spawns one coordinator node (label `pipeline <name>`, no model) as the workflow root. Its steps are its children. The coordinator counts against the tree-global `max_spawns` like any node. The definition-level `workflow:` field stays advisory. The tool argument, when set, governs and overwrites `p.Workflow`.
3. A worker-bound child (a `worker` was selected inside a workflow) always gets the five blackboard tools, whatever its allowlist says (the spec's "plus the always-wired blackboard tools"). This also changes research-probe `facet-researcher` children, which previously had no blackboard tools. That is the intended single path.
4. `worker` outside any workflow subtree stays ignored, as it is today (pipeline steps outside a workflow keep working).
5. `spawn_agent` advertises `workflow` only when the spawning node is outside a workflow subtree, and advertises `worker` only inside one. A model that sends `workflow` anyway inside a subtree gets the nested refusal.
6. Unknown workflow and unknown worker are refused in the spawn func before the Spawner runs. No node is created and no quota is spent. `prepareChild` repeats the checks as defense for paths that call `Spawner.Spawn` directly (pipeline steps).

---

## Task 1: Agent - `SpawnOpts.Workflow`, nested refusal, `WithBuilder`

**Tier:** standard

**Files:**
- Modify: `internal/agent/spawn.go`
- Create: `internal/agent/spawn_workflow_test.go`

**Step 1: Write failing tests** in `internal/agent/spawn_workflow_test.go`. Reuse the stub child-builder pattern from `internal/agent/spawn_test.go` (a builder returning a fixed string). Tests:
- `TestSpawnWorkflowMarksChildAsWorkflowRoot`: tree `NewAgentTree("root","m")`, spawner at the root with a stub builder. `Spawn(ctx, SpawnOpts{Label:"lead", Task:"t", Workflow:"wf"})`, then `h.Wait()`. Assert `tree.Node(h.NodeID).Snapshot().WorkflowRoot == "wf"` and `tree.WorkflowRootOf(h.NodeID) == h.NodeID`.
- `TestSpawnWithoutWorkflowLeavesMarkerEmpty`: same, no `Workflow`. `WorkflowRoot == ""`.
- `TestSpawnRefusesNestedWorkflow`: spawn `lead` with `Workflow:"wf"`. Build a second spawner at `tree.Node(lead.NodeID)` (depth 1). `Spawn(SpawnOpts{Label:"n", Task:"t", Workflow:"wf2"})` returns an error with `errors.Is(err, ErrNestedWorkflow)`. `len(tree.SnapshotAll())` is unchanged (no node added). Also assert a grandchild spawned from inside the subtree WITHOUT `Workflow` succeeds.
- `TestWithBuilderReplacesOnlyTheChildBuilder`: spawner with builder A, then `sp.WithBuilder(B).Spawn(...)` runs B, and `sp.Spawn(...)` still runs A. The original is not mutated.

**Step 2: Run them, confirm they fail:** `go test ./internal/agent -run 'TestSpawnWorkflow|TestSpawnWithoutWorkflow|TestSpawnRefusesNested|TestWithBuilder' -count=1` (compile failure on `Workflow`, `ErrNestedWorkflow` and `WithBuilder` is the expected red).

**Step 3: Implement** in `internal/agent/spawn.go`:
- Add to `SpawnOpts` after `Worker`:
  ```go
  // Workflow names a workflow the child becomes the ROOT of (change 0091). The
  // Spawner marks the new node's WorkflowRoot at creation; the binding's child
  // builder registers the pool. Empty for every other spawn. Refused from inside
  // an active workflow subtree (ErrNestedWorkflow).
  Workflow string
  ```
- Next to `ErrWorkflowQuotaExhausted`:
  ```go
  // ErrNestedWorkflow refuses a workflow activation requested from inside an
  // active workflow subtree (change 0091): workflows do not nest.
  var ErrNestedWorkflow = errors.New("agent: nested workflow activation refused")
  ```
- In `Spawn`, directly after the `MaxDepth` check:
  ```go
  if opts.Workflow != "" && s.tree != nil && s.node != nil {
      if rootID := s.tree.WorkflowRootOf(s.node.ID); rootID != "" {
          active := ""
          if n := s.tree.Node(rootID); n != nil {
              active = n.Snapshot().WorkflowRoot
          }
          return AgentHandle{}, fmt.Errorf("%w: cannot start workflow %q inside workflow %q", ErrNestedWorkflow, opts.Workflow, active)
      }
  }
  ```
- In `spawnLocal`'s `&AgentNode{...}` literal, add `WorkflowRoot: opts.Workflow,`. Update the comment above `poolID` ("a freshly-spawned child never carries its own workflow-root label") to say that a workflow-root child may carry one, but it still queues into its parent's pool because `poolID` is derived from `s.node`.
- Update the `AgentNode.WorkflowRoot` doc comment in `internal/agent/tree.go`: it is set at node creation from `SpawnOpts.Workflow`, or on a not-yet-running root by the research probe. Never mutated after.
- Add:
  ```go
  // WithBuilder returns a copy of the Spawner whose children run through fn
  // instead (change 0091). Tree, node, depth, backstop, bus, store and observer
  // are shared; the receiver is unchanged.
  func (s *Spawner) WithBuilder(fn ChildBuilder) *Spawner {
      cp := *s
      cp.buildChild = fn
      return &cp
  }
  ```
  (`Spawner` holds no lock, so a shallow copy is safe. Confirm this while editing.)

**Step 4: Run** `go test ./internal/agent -count=1 -race`. All pass.

**Step 5: Commit** `feat(agent): mark workflow roots at spawn and refuse nested activation`

---

## Task 2: Tools - `spawn_agent` `workflow` argument

**Tier:** economy

**Files:**
- Modify: `internal/tools/spawn_agent.go`
- Modify: `internal/tools/spawn_agent_test.go`

**Step 1: Failing tests** in `spawn_agent_test.go` (follow the existing `WithWorkers` enum test near line 203):
- `TestSpawnAgentWorkflowParamAdvertisesNames`: `NewSpawnAgentTool(okSpawn("x")).WithWorkflows([]string{"wf","research"})`. `Parameters()["properties"]["workflow"]` exists, its `enum` is `["research","wf"]` (sorted), and its description contains both names. `Description()` contains `research` and `wf`.
- `TestSpawnAgentNoWorkflowsNoParam`: without `WithWorkflows`, there is no `workflow` property and `Description()` is byte-identical to today's string.
- `TestSpawnAgentForwardsWorkflow`: a capturing `SpawnFunc`. `Execute(ctx, `{"label":"l","task":"t","workflow":"wf"}`)` gives `req.Workflow == "wf"`. Do this even with no `WithWorkflows` set, since the scope validates names, not the tool.

**Step 2:** `go test ./internal/tools -run TestSpawnAgent -count=1`. Red.

**Step 3: Implement:**
- `SpawnRequest.Workflow string` (doc: workflow the child roots, change 0091).
- `SpawnAgentTool.workflows []string`. Add `WithWorkflows(names []string) *SpawnAgentTool` (copy, sorted copy of names).
- `Description()`: when `len(t.workflows) > 0`, append `" Pass `workflow` to run the child as the root of a configured workflow (" + strings.Join(t.workflows, ", ") + "); its subtree is then governed by that workflow's pool and typed workers."`. Otherwise return the current string unchanged.
- `Parameters()`: when `len(t.workflows) > 0`, add `"workflow": {"type":"string","enum":[...],"description":"Optional workflow the child becomes the root of: <names>. Its pool limits and typed workers then govern the child's subtree. Not allowed inside a workflow."}`.
- `spawnAgentInput.Workflow string `json:"workflow"`` and forward it in `Execute`.

**Step 4:** `go test ./internal/tools -count=1`. Green.

**Step 5: Commit** `feat(tools): spawn_agent accepts and advertises a workflow argument`

---

## Task 3: Tools - `pipeline_run` `workflow` argument (seam signature)

**Tier:** standard

**Files:**
- Modify: `internal/tools/pipeline_run.go`, `internal/tools/pipeline_run_test.go`
- Modify: `cmd/fuse/run.go` (`makePipelineRunFn`, `makePipelineSynthFn`, `wirePipelineTool`)
- Modify (compile only): `cmd/fuse/pipeline_wiring_test.go`, `cmd/fuse/pipeline_gateway_e2e_test.go`, `internal/tui/pipeline_tui_e2e_test.go`

**Step 1: Failing tests** in `pipeline_run_test.go`:
- `TestPipelineRunForwardsWorkflow`: the run seam captures `workflow` for `{"definition":"...","workflow":"wf"}`. The synth seam captures it for `{"goal":"g","workflow":"wf"}`.
- `TestPipelineRunWorkflowParamAdvertised`: `NewPipelineRunTool(run, synth).WithWorkflows([]string{"wf"})` has a `workflow` property with enum `["wf"]`. Without it there is no property.

**Step 2:** `go test ./internal/tools -run TestPipelineRun -count=1`. Red.

**Step 3: Implement:**
- `type PipelineRunFunc func(ctx context.Context, definition []byte, workflow string) (statusText string, err error)`
- `type PipelineSynthFunc func(ctx context.Context, goal string, confirm bool, workflow string) (statusText string, err error)`
- Doc both: `workflow` is the optional workflow the pipeline's steps run under (change 0091); "" keeps today's behavior.
- `PipelineRunTool.workflows []string`, `WithWorkflows(names)` (copy, sorted). In `Parameters()` add `workflow` (enum and description listing names: "Optional workflow to run the pipeline under: <names>. Its steps' `worker` names resolve to that workflow's workers and its pool limits apply.") only when names are set. `pipelineRunInput.Workflow`, forwarded to both seams.
- `cmd/fuse/run.go`: give the closures returned by `makePipelineRunFn` and `makePipelineSynthFn` the new parameter. For THIS task, a non-empty workflow returns `fmt.Errorf("workflow %q: workflow pipelines are not available in this context", workflow)` (Task 5 replaces this). Split `wirePipelineTool` so a pre-built tool can be wired:
  ```go
  func wirePipelineTool(reg *tools.Registry, runFn tools.PipelineRunFunc, synthFn tools.PipelineSynthFunc, requested []string) {
      wireBuiltPipelineTool(reg, tools.NewPipelineRunTool(runFn, synthFn), requested)
  }
  // wireBuiltPipelineTool applies wirePipelineTool's include rule to a built tool (change 0091).
  func wireBuiltPipelineTool(reg *tools.Registry, t *tools.PipelineRunTool, requested []string) { ...existing body using t... }
  ```
- Update the func literals in the three test files to the new signatures (add the `workflow string` parameter, ignore it).

**Step 4:** `go build ./... && go vet ./... && go test ./internal/tools ./internal/tui ./cmd/fuse -run 'Pipeline' -count=1`. Green.

**Step 5: Commit** `feat(tools): pipeline_run accepts and advertises a workflow argument`

---

## Task 4: `workflowScope` - activation, lookup, validation, worker plan

**Tier:** premium (the quota counter and activation keying carry the enforcement)

**Files:**
- Modify: `cmd/fuse/workflow.go`, `cmd/fuse/run.go` (`spawnFuncFrom` forwards `Workflow`)
- Create: `cmd/fuse/workflow_scope_test.go`

**Step 1: Failing tests** in `workflow_scope_test.go`. Build a real `agent.NewAgentTreeWithConcurrency("m","m",4)` and `wfCfg := map[string]config.WorkflowConfig{"wf": {Pool: config.PoolConfig{Total: 2}, Workers: map[string]config.WorkerConfig{"reader": {Tools: []string{"read_file"}, Model: "pin"}}}}`. Obtain real child nodes by spawning through `agent.NewSpawner(WithTree, WithNode(root), WithChildBuilder(stub))`, where the stub calls `scope.prepareChild(opts, childNode)` and records the result and node.
- `TestScopeNamesSortedAndCheck`: `names()` is sorted. `check("nope")` errors with `unknown workflow "nope" (available: wf)`. With no workflows the message says `(none configured)`. `check("wf") == nil`.
- `TestPrepareChildActivatesWorkflow`: spawn with `Workflow:"wf"`. In the builder, `prepareChild` succeeds, `tree.Scheduler().Pool(child.ID)` returns `Total: 2`, and `activationOf(child.ID)` returns a non-nil activation with root id `child.ID` and `name == "wf"`.
- `TestPrepareChildResolvesWorkerInSubtree`: from a spawner at the activated lead (`scope.backstop(lead)` installed), spawn `Worker:"reader"`. The plan has `tools == ["read_file"]`, `bound == true`, `model == "pin"`. `Worker:"ghost"` returns an error containing `unknown worker "ghost"`.
- `TestPrepareChildIgnoresWorkerOutsideWorkflow`: root-level spawn with `Worker:"reader"` and no workflow gives `tools == opts.Tools`, `bound == false`.
- `TestScopeBackstopEnforcesTotal`: after activating the lead, `bs := scope.backstop(leadNode)`. Calls 1 and 2 return nil and call 3 returns `errors.Is(err, agent.ErrWorkflowQuotaExhausted)`. Calling `scope.backstop(leadNode)` again shares the same counter (call 4 is still refused). A backstop for a node outside any workflow returns nil.
- `TestScopeSpawnFuncRefusesBeforeSpawn`: `scope.spawnFunc(spawner, root)` with `SpawnRequest{Label:"x", Task:"t", Workflow:"nope"}` returns an `unknown workflow` error, and the node count is unchanged. Inside the lead, `Worker:"ghost"` is refused the same way. Root-level `Workflow:"wf", Worker:"ghost"` is refused (the worker resolves against the requested workflow).
- `TestScopeSpawnToolAdvertisesByPosition`: `scope.spawnTool(fn, root)` has a `workflow` property and no `worker` property. `scope.spawnTool(fn, lead)` has `worker` (enum `["reader"]`) and no `workflow`.
- `TestActivateRootMarksRoot`: on a fresh tree, `scope.activateRoot(rootNode, "wf")` sets `rootNode.WorkflowRoot == "wf"` and registers the pool. An unknown name errors.

**Step 2:** `go test ./cmd/fuse -run 'TestScope|TestPrepareChild|TestActivateRoot' -count=1`. Red.

**Step 3: Implement** in `cmd/fuse/workflow.go` (keep `workflowActivation`, `resolveWorkerTools`, `backstopFor`, `budgetFor` and `quotaWarningFor`; existing `workflow_test.go` must stay green):
```go
// workflowScope is one loop's workflow activation state (change 0091). Every
// binding builds one per agent tree; activations are keyed by the workflow-root
// node id, so two roots of the same workflow each get their own pool and quota.
type workflowScope struct {
    tree      *agent.AgentTree
    workflows map[string]config.WorkflowConfig
    mu        sync.Mutex
    active    map[string]*workflowActivation
}

func newWorkflowScope(tree *agent.AgentTree, workflows map[string]config.WorkflowConfig) *workflowScope

// childPlan is the workflow-resolved shape of one child spawn.
type childPlan struct {
    tools []string // effective tool list (nil = inherit all)
    model string   // worker model pin, "" when none
    bound bool     // a worker was selected inside a workflow
}
```
Methods:
- `names() []string`: sorted keys of `workflows`.
- `check(name string) error`: unknown gives `fmt.Errorf("unknown workflow %q (available: %s)", name, list)`, with `list` = `strings.Join(names, ", ")` or `none configured`.
- `activateWorkflow(node *agent.AgentNode) (*workflowActivation, error)`: `name := node.Snapshot().WorkflowRoot`. Empty is an error. Run `check(name)`. Under `mu`, return the existing activation for `node.ID` if present (idempotent). Otherwise create `&workflowActivation{name, cfg: workflows[name], rootDepth: node.Depth}`, call `tree.Scheduler().RegisterPool(node.ID, act.pool())`, store and return it.
- `activateRoot(node *agent.AgentNode, name string) (*workflowActivation, error)`: the one root-level case (research probe). Run `check(name)`, then set `node.WorkflowRoot = name` (document: only before the root runs, matching the create-once invariant), then `activateWorkflow(node)`.
- `activationOf(nodeID string) (*workflowActivation, string)`: `rootID := tree.WorkflowRootOf(nodeID)`. "" gives `nil, ""`. Otherwise return the stored activation. If that root is not yet activated (a path that skipped `prepareChild`), activate it lazily via `activateWorkflow(tree.Node(rootID))`, and on error return `nil, ""`.
- `backstop(parent *agent.AgentNode) func(newDepth int) error`: lazy. Per call, resolve `activationOf(parent.ID)` and delegate to `backstopFor(tree, act, rootID)` when non-nil.
- `budget(node *agent.AgentNode) tools.BudgetFunc`: `budgetFor(tree, act, rootID)` from `activationOf(node.ID)`.
- `spawnTool(fn tools.SpawnFunc, node *agent.AgentNode) *tools.SpawnAgentTool`: `tools.NewSpawnAgentToolWithBudget(fn, s.budget(node)).WithQuotaWarning(quotaWarningFor(s.tree, node.ID))`, then `.WithWorkers(act.workerNames())` when inside a workflow, else `.WithWorkflows(s.names())`. Sort `workerNames()` output for a stable schema.
- `checkRequest(parent *agent.AgentNode, workflow, worker string) error`: if `workflow != ""`, run `check(workflow)`, and when `worker != ""` it must exist in `workflows[workflow].Workers`. Else if `worker != ""` and `activationOf(parent.ID)` is non-nil, the worker must exist in that activation's workers. Unknown gives `fmt.Errorf("unknown worker %q", worker)` (same text as `resolveWorkerTools`). The nested case is left to the Spawner (`ErrNestedWorkflow`).
- `spawnFunc(sp *agent.Spawner, parent *agent.AgentNode) tools.SpawnFunc`: run `checkRequest(parent, req.Workflow, req.Worker)` first, then delegate to `spawnFuncFrom(sp, s.tree.Scheduler(), parent)`.
- `prepareChild(opts agent.SpawnOpts, child *agent.AgentNode) (childPlan, error)`: if `opts.Workflow != ""`, call `activateWorkflow(child)`. Then `act, _ := activationOf(child.ID)`. If `act == nil`, return `childPlan{tools: opts.Tools}`. Else `rt, err := resolveWorkerTools(act.cfg, opts.Worker, opts.Tools)`, `bound := opts.Worker != ""`, and `model` = `act.cfg.Workers[opts.Worker].Model` when bound.

In `cmd/fuse/run.go` `spawnFuncFrom`, add `Workflow: req.Workflow,` to the `agent.SpawnOpts` literal (and a `spawn_func_from_test.go` assertion that it is forwarded).

**Step 4:** `go test ./cmd/fuse -run 'TestScope|TestPrepareChild|TestActivateRoot|TestResolveWorker|TestBackstop|TestBudget|TestWorker|SpawnFuncFrom' -count=1 -race`. Green.

**Step 5: Commit** `feat(fuse): per-loop workflow scope for per-spawn activation`

---

## Task 5: Shared child and root wiring, plus workflow pipelines

**Tier:** premium

**Files:**
- Create: `cmd/fuse/loop_wiring.go`, `cmd/fuse/loop_wiring_test.go`
- Modify: `cmd/fuse/workflow.go` (`runner`), `cmd/fuse/run.go` (`makePipelineRunFn`, `makePipelineSynthFn`)

**Step 1: Failing tests** in `loop_wiring_test.go`. Set up `cfg` with the Task 4 `wf`, a base registry `defaultToolRegistry(nil, cfg.Research, nil)`, a tree, `bb := agent.NewBlackboard(tree)`, `scope := newWorkflowScope(tree, cfg.Workflows)`, and a `makeSpawner` whose child builder is a stub that calls `w.childTools(base, opts, childNode)` and records the registry's `Schemas()` names.
- `TestChildToolsWorkerBoundSchemaIsAllowlistPlusBlackboard`: root-level spawn `Workflow:"wf"` (lead). From the lead's spawner, spawn `Worker:"reader"`. The recorded schema names, sorted, are exactly `[blackboard_delete blackboard_keys blackboard_read blackboard_wait blackboard_write read_file]`.
- `TestChildToolsFreeformKeepsTodayShape`: root-level spawn with no workflow or worker. The schema contains `spawn_agent` (with a `workflow` property), `pipeline_run` (with a `workflow` property) and all five blackboard tools. With `Tools: ["read_file"]` the schema is exactly `[read_file]` (unchanged subset semantics).
- `TestChildToolsInsideWorkflowAdvertisesWorkers`: the lead's own registry has `spawn_agent` with a `worker` enum and no `workflow` property, and `pipeline_run` has no `workflow` property.
- `TestRootToolsRegistersScopedTools`: `w.rootTools(reg, rootNode)` registers `spawn_agent` (with `workflow`), five blackboard tools and `pipeline_run` (with `workflow`).
- `TestRunnerRunsStepsUnderCoordinatorRoot`: `run := scope.runner(makeSpawner, rootNode)`. Call `run(ctx, "pipeline p", "wf", fn)`, where `fn` spawns one child with `Worker:"reader"` from the `steps` spawner and waits. Afterwards: exactly one node labelled `pipeline p` with `WorkflowRoot == "wf"`, whose parent is the root. The step node's `WorkflowRootOf` is the coordinator. `Pool(coord.ID).Total == 2`. The step schema is the reader allowlist plus blackboard. A fourth case runs `run` from a node inside a workflow and gets `ErrNestedWorkflow`. `run(ctx, "pipeline p", "nope", fn)` gets `unknown workflow`, `fn` is never called, and no coordinator node is created.
- `TestPipelineRunFnWorkflowUsesRunner`: `makePipelineRunFn(spawner, bb, sched, rootNode, scope.runner(makeSpawner, rootNode))` with definition `name: p\nsteps:\n  - name: s1\n    worker: reader\n    prompt: hi\n` and workflow `wf` returns `pipeline p: completed`, and a coordinator node exists. With workflow "", there is no coordinator (today's path). With a nil runner and workflow `wf`, there is an error containing `not available`.

**Step 2:** `go test ./cmd/fuse -run 'TestChildTools|TestRootTools|TestRunner|TestPipelineRunFnWorkflow' -count=1`. Red.

**Step 3: Implement.**

`cmd/fuse/workflow.go`:
```go
// workflowRunner runs fn with a spawner rooted at a fresh coordinator node that
// roots workflow (change 0091): the pipeline_run workflow path.
type workflowRunner func(ctx context.Context, label, workflow string, fn func(ctx context.Context, steps *agent.Spawner) (string, error)) (string, error)

func (s *workflowScope) runner(makeSpawner func(*agent.AgentNode, int) *agent.Spawner, parent *agent.AgentNode) workflowRunner
```
Body: run `check(workflow)` first. `coord := makeSpawner(parent, parent.Depth).WithBuilder(func(ctx, _ agent.SpawnOpts, node *agent.AgentNode, _ *agent.AgentTree) (string, error) { act; sched.YieldSlot(node); out, err := fn(ctx, makeSpawner(node, node.Depth)); if !sched.UnyieldSlot(ctx, node) { return "", ctx.Err() }; return out, err })`, where `act` is `if _, err := s.activateWorkflow(node); err != nil { return "", err }`. Then `h, err := coord.Spawn(ctx, agent.SpawnOpts{Label: label, Task: label, Workflow: workflow})`. On error return it. Otherwise `d := h.Wait(); return d.Result, d.Err`. Comment the slot yield with the learning name (the coordinator runs no model, so holding a slot while its steps queue deadlocks under `max_concurrent`).

`cmd/fuse/run.go`:
- `makePipelineRunFn(spawner, bb, sched, node, runWF workflowRunner)`. After parse and validate: if `workflow != ""` and `runWF == nil`, return `fmt.Errorf("workflow %q: workflow pipelines are not available in this context", workflow)`. Keep the caller's `YieldSlot`/`UnyieldSlot` around the whole call. With a workflow, set `p.Workflow = workflow` and `line, err = runWF(ctx, "pipeline "+p.Name, workflow, func(ctx, steps) (string, error) { st, _ := pipeline.Run(ctx, p, steps, bb); return pipelineStatusLine(p.Name, st), nil })`. Without one, keep today's path exactly.
- `makePipelineSynthFn(..., traceW io.Writer, runWF workflowRunner)`. With a workflow, first refuse an unknown name early (`cfg.Workflows[workflow]` missing) using the same message as `workflowScope.check`. Share one package helper, `unknownWorkflowErr(name string, names []string) error`, between the two. Then synthesize from the caller's `spawner` with `SynthContext.Workers` = sorted worker names of `cfg.Workflows[workflow]`. Set `p.Workflow = workflow` before publishing the plan, then run the steps through `runWF` as above. Without a workflow, behavior is unchanged.
- Update the existing callers in tests to pass `nil` for `runWF`.

`cmd/fuse/loop_wiring.go`:
```go
// loopWiring is the per-loop tool wiring every binding's root and child builder
// shares (change 0091): the workflow scope, worker allowlists, scoped spawn_agent,
// blackboard and pipeline_run all come from one path, so no host can leave a
// workflow's pool or allowlist inert (learnings patch-every-cloned-child-builder,
// security-knob-inert-at-composition-root).
type loopWiring struct {
    cfg         config.Config
    tree        *agent.AgentTree
    bb          *agent.Blackboard
    scope       *workflowScope
    makeSpawner func(parent *agent.AgentNode, depth int) *agent.Spawner
    traceW      io.Writer
}

// childTools builds childNode's registry from base and returns its worker model pin.
func (w loopWiring) childTools(base *tools.Registry, opts agent.SpawnOpts, childNode *agent.AgentNode) (*tools.Registry, string, error)

// rootTools registers rootNode's spawn_agent, blackboard and pipeline_run on reg.
func (w loopWiring) rootTools(reg *tools.Registry, rootNode *agent.AgentNode)
```
`childTools` body, moved from the current closures with no other behavior change:
1. `plan, err := w.scope.prepareChild(opts, childNode)`.
2. `reg, err := childToolRegistry(base, plan.tools)`.
3. If `childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(plan.tools)`, run `reg.Unregister("spawn_agent")`. Else `reg.Register(w.scope.spawnTool(w.scope.spawnFunc(w.makeSpawner(childNode, childNode.Depth), childNode), childNode))`.
4. `bbReq := plan.tools`, set to `nil` when `plan.bound` (decision 3). Then `wireChildBlackboard(reg, w.bb, childNode, bbReq)`.
5. `w.wirePipeline(reg, childNode, plan.tools)`.
6. Return `reg, plan.model, nil`.

`rootTools`: `reg.Register(w.scope.spawnTool(w.scope.spawnFunc(w.makeSpawner(rootNode, 0), rootNode), rootNode))`, then `wireRootBlackboard(reg, w.bb, rootNode)`, then `w.wirePipeline(reg, rootNode, nil)`.

`wirePipeline(reg, node, requested)`: `workers, toolNames := pipelineSynthPalette(w.cfg, reg)`, `sp := w.makeSpawner(node, node.Depth)`, `run := w.scope.runner(w.makeSpawner, node)`, `t := tools.NewPipelineRunTool(makePipelineRunFn(sp, w.bb, sched, node, run), makePipelineSynthFn(sp, w.bb, sched, node, w.cfg, workers, toolNames, w.traceW, run))`. If `act, _ := w.scope.activationOf(node.ID); act == nil`, then `t = t.WithWorkflows(w.scope.names())`. Finally `wireBuiltPipelineTool(reg, t, requested)`. Note: the root uses `makeSpawner(rootNode, 0)`, and `rootNode.Depth == 0`, so `node.Depth` is equivalent. Keep the palette computed before `pipeline_run` is registered (today's order).

**Step 4:** `go test ./cmd/fuse -count=1 -race -run 'TestChildTools|TestRootTools|TestRunner|TestPipeline|TestScope|TestWorkflow'`. Green. Then `go build ./...`.

**Step 5: Commit** `feat(fuse): shared loop wiring for workflow-scoped child tools and pipelines`

---

## Task 6: Adopt the shared wiring in every binding, and re-point the source guards

**Tier:** premium (four composition roots; any site left on the old path keeps the defect)

**Files:**
- Modify: `cmd/fuse/runtime_binding.go` (one-shot, research probe, shell), `cmd/fuse/loop_server.go` (loop factory), `cmd/fuse/research_probe.go`
- Modify: `cmd/fuse/pipeline_wiring_test.go` (`TestEveryAgentEntryPointWiresPipeline`), `cmd/fuse/blackboard_wiring_test.go` (the guard near line 187), and the `researchProbeDepsInput{...}` literals in `shared_registry_teardown_test.go`, `two_bindings_parity_test.go` and `observe_wiring_test.go` (two sites)
- Create: `cmd/fuse/workflow_wiring_guard_test.go`

**Step 0:** Re-derive the sites: `grep -n 'childBuilder := func\|agent.WithChildBuilder(\|NewSpawnAgentToolWithBudget(\|wireChildBlackboard(\|wirePipelineTool(' cmd/fuse/*.go | grep -v _test`. Every non-test hit outside `loop_wiring.go`, `run.go` (definitions) and `workflow.go` must be migrated.

**Step 1: Failing guard test** `workflow_wiring_guard_test.go`, `TestEveryChildBuilderUsesSharedWorkflowWiring`. For each non-test `.go` file in `cmd/fuse`: `n := strings.Count(src, "agent.WithChildBuilder(")`. When `n > 0`, require `strings.Count(src, ".childTools(") >= n` and `strings.Count(src, ".rootTools(") >= n`. In every non-test file except `loop_wiring.go`, `workflow.go` and `run.go`, forbid the direct calls `childToolRegistry(`, `wireChildBlackboard(`, `wirePipelineTool(`, `NewSpawnAgentToolWithBudget(` and `resolveWorkerTools(`. Require total sites `>= 4`. Rewrite `TestEveryAgentEntryPointWiresPipeline` and the blackboard guard to the same shape: files with child-builder closures must call `.childTools(`/`.rootTools(`, and `loop_wiring.go` must call `wirePipeline`/`wireChildBlackboard`/`wireRootBlackboard`. Run: red.

**Step 2: Migrate each binding.** Each follows the same pattern:
```go
scope := newWorkflowScope(tree, cfg.Workflows)       // loop server: inside BuildAgent, per loop tree
var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
wiring := loopWiring{cfg: cfg, tree: tree, bb: bb, scope: scope, traceW: traceW /* nil on loop server */,
    makeSpawner: func(p *agent.AgentNode, d int) *agent.Spawner { return makeSpawner(p, d) }}
childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
    childToolReg, workerModel, err := wiring.childTools(<base registry>, opts, childNode)
    if err != nil { return "", err }
    modelID := opts.ModelID
    if modelID == "" { modelID = workerModel }   // worker model pin (change 0034), now every binding
    if modelID == "" { modelID = <binding default alias> }
    ... binding-specific renderer / approvals / agent build / Set* calls, unchanged ...
}
makeSpawner = func(parentNode *agent.AgentNode, depth int) *agent.Spawner {
    return agent.NewSpawner(
        ...existing options...,
        agent.WithSpawnBackstop(scope.backstop(parentNode)),
        agent.WithChildBuilder(childBuilder),
    )
}
wiring.rootTools(<root registry>, rootNode)
```
Per site:
- One-shot (`buildOneShotRuntimeDeps`): base is `toolReg`, `traceW` is `traceW`. Replace the root `spawn_agent`/blackboard/pipeline block with `wiring.rootTools(toolReg, rootNode)`.
- Shell (`buildShellRuntimeDeps`): base is `toolReg`. Keep `handleReg.Register`, `sessLog` and the human injector. Model default `alias`.
- Research probe: replace `researchProbeDepsInput.act`/`rootID` with `scope *workflowScope`. When nil, use `newWorkflowScope(tree, cfg.Workflows)`. In `runResearchProbe` (`research_probe.go`), replace the inline activation with `scope := newWorkflowScope(tree, cfg.Workflows)` and, when `cfg.Workflows["research"]` exists, `if _, err := scope.activateRoot(rootNode, "research"); err != nil { fmt.Fprintf(stderr, "research-probe: %v\n", err); return 1 }`. Pass `scope`. Delete the probe's own `resolveWorkerTools`/`WithWorkers`/`backstopFor`/`budgetFor` calls (now inside the scope). Update the four test literals (drop `act:`/`rootID:`).
- Loop server (`loopServerHost.factory` `BuildAgent`): base is `loopToolReg`, `traceW` is nil, `scope := newWorkflowScope(tree, cfg.Workflows)` per loop (cfg is the loop's resolved config, so spec workflows apply). `wiring.rootTools(loopToolReg, rootNode)`.

Update each binding's "cloned child-builder site N of 3" comments to "site N of 4, wired through loopWiring".

**Step 3:** `go build ./... && go vet ./... && go test ./cmd/fuse -count=1 -race`. Green, including the existing research-probe, parity, observe, teardown and pipeline e2e tests. If a golden or exact-schema assertion changes only because `spawn_agent`/`pipeline_run` now carry the `workflow` property (or facet-researchers now carry blackboard tools), update that expectation and say so in the commit body. Any other diff is a regression to fix, not to re-golden.

**Step 4: Research-probe root assertion** (add to `workflow_wiring_guard_test.go` or `runtime_binding_test.go`): `TestResearchProbeRootActivatesThroughScope`. Build the probe deps the way `observe_wiring_test.go` `TestResearchProbeBindingHonorsSuppliedObserver` does, with a scope where `activateRoot(rootNode, "research")` ran. `tree.Scheduler().Pool(rootID)` is registered. The root registry's `spawn_agent` schema has a `worker` enum `["facet-researcher"]` and no `workflow` property. Red before Step 2, green after.

**Step 5: Commit** `fix(fuse): every binding activates workflows per spawn through one wiring path`

---

## Task 7: Local-binding acceptance (one-shot, shell, research probe)

**Tier:** standard

**Files:**
- Create: `cmd/fuse/workflow_activation_test.go` (shared scripted-gateway helpers live here and are reused by Task 8)

**Step 1: Helpers** (test-only). Do not reuse the name `scriptedGateway` (a func by that name exists in `observe_wiring_test.go`).
```go
// routedGateway answers each chat request by routing on its first user message
// and how many assistant turns it already carries, and records every request.
type routedGateway struct {
    mu    sync.Mutex
    calls []gatewayCall           // gatewayCall is defined in loop_harness_test.go
    route func(task string, turn int, c gatewayCall) string
}
func (g *routedGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) // decode, record, compute task (first role=="user" content) and turn (count of role=="assistant"), write route(...)
func (g *routedGateway) callsFor(marker string) []gatewayCall           // requests whose first user message contains marker
type call struct{ name string; args map[string]any }
func toolCallsReply(calls ...call) string // json.Marshal: {"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c<i>","type":"function","function":{"name":..,"arguments":<string(json.Marshal(args))>}}]}}]}
func textReply(s string) string           // {"choices":[{"message":{"role":"assistant","content":s}}]}
func argsJSON(m map[string]any) string    // string(json.Marshal(m)): plain JSON for Registry.Execute
func toolResults(c gatewayCall) []string  // contents of role=="tool" messages
func sortedToolNames(c gatewayCall) []string
```
Use unique markers in task strings (`LEAD-TASK`, `READ-TASK`, `STEP-TASK`) and match with `strings.Contains`. Build `arguments` only through `json.Marshal` (learning `scripted-gateway-double-double-escapes-tool-args`).

Lead script used by Tasks 7 and 8: `LEAD-TASK` turn 0 sends three parallel `spawn_agent` calls `{label:"r<i>", task:"READ-TASK", worker:"reader"}`. Later turns send `textReply("lead done")`. `READ-TASK` always sends `textReply("read done")`.

**Step 2: Failing test** `TestLocalBindingsActivateWorkflowPerSpawn`, table-driven over `one-shot` and `shell`. Per case:
- `cfg, _ := config.FromYAML([]byte(wfOperatorYAML), "test")`, where `wfOperatorYAML` is `harnessOperator` with `max_turns: 4` plus `workflows: {wf: {pool: {total: 2}, workers: {reader: {tools: [read_file]}}}}`. Set `cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"` (srv wraps the `routedGateway`), `t.Setenv("HOME", t.TempDir())`, `reg := registryFromConfig(cfg)`, `toolReg := defaultToolRegistry(nil, cfg.Research, nil)` and a tree. Build deps with the binding's real builder (argument shapes as in `observe_wiring_test.go` `TestBindingsDefaultToNoopObserver`).
- Drive the binding's real root tool: `res := toolReg.Execute(ctx, "spawn_agent", argsJSON(map[string]any{"label": "lead", "task": "LEAD-TASK", "workflow": "wf"}))`. Assert `!res.IsError`.
- `len(gw.callsFor("READ-TASK")) == 2`, and each has `sortedToolNames == [blackboard_delete blackboard_keys blackboard_read blackboard_wait blackboard_write read_file]`.
- The lead's turn-1 request carries three tool results: exactly one contains `workflow spawn quota exhausted`, and two contain `read done`.
- The tree has a node labelled `lead` with `Snapshot().WorkflowRoot == "wf"`, and `tree.Scheduler().Pool(lead.ID)` has `Total == 2`. Every reader's `WorkflowRootOf` is the lead.

Second test `TestResearchProbeRefusesNestedRootSpawn`: probe deps over the default config with the root activated (`activateRoot(rootNode, "research")`). Running `toolReg.Execute(ctx, "spawn_agent", argsJSON(map[string]any{"label": "x", "task": "X-TASK", "workflow": "research"}))` is `IsError` and contains `nested workflow activation refused`. No request carries `X-TASK`.

Run: `go test ./cmd/fuse -run 'TestLocalBindingsActivateWorkflowPerSpawn|TestResearchProbeRefusesNested' -count=1 -race`. After Task 6 this should pass. If it fails, the failure points at a wiring gap. Fix the wiring, never the assertion.

**Step 3: Commit** `test(fuse): local bindings enforce workflow pools and worker tools per spawn`

---

## Task 8: Hosted acceptance through the production loop-server factory

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/loop_harness_test.go` (split `harnessServer`)
- Create: `cmd/fuse/loop_server_workflow_test.go`

**Step 1: Helper refactor.** Split `harnessServer` without changing its behavior:
```go
// harnessServerWith builds the production loop-server deps over gw.
func harnessServerWith(t *testing.T, operatorYAML string, gw http.Handler) (runtime.Deps, *loopServerHost)
func harnessServer(t *testing.T, operatorYAML string) (runtime.Runtime, *recordingGateway, *loopServerHost) {
    gw := &recordingGateway{}
    deps, host := harnessServerWith(t, operatorYAML, gw)
    return runtime.New(deps), gw, host
}
```
Add a test-only tree-capturing resolver that wraps the production one:
```go
type treeCapture struct {
    inner runtime.HarnessResolver
    mu    sync.Mutex
    trees []*agent.AgentTree
}
func (c *treeCapture) ResolveHarness(ctx context.Context, tenant event.TenantID, spec *harnessv1.HarnessSpec) (runtime.LoopFactory, error)
// delegates, then wraps f.BuildAgent to record the tree before calling the original
func (c *treeCapture) last() *agent.AgentTree
```
Usage: `deps, host := harnessServerWith(t, op, gw); cap := &treeCapture{inner: host}; deps.Harness = cap; rt := runtime.New(deps)`.

The spec for all three tests (a spec-defined workflow, ADR-0061):
```go
spec := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{
    "wf": {Pool: &harnessv1.Pool{Total: 2}, Workers: map[string]*harnessv1.Worker{"reader": {Tools: []string{"read_file"}}}},
}}
```
Operator YAML is `harnessOperator` with `max_turns: 4`. Start loops with `rt.StartLoop(ctx, runtime.LoopConfig{Task: "ROOT-TASK", Harness: spec})` and `h.Wait()`.

**Step 2: Failing tests** in `loop_server_workflow_test.go`:
- `TestHostedWorkflowSpawnEnforcesPoolAndWorkerTools`: `ROOT-TASK` turn 0 is `spawn_agent{label:"lead", task:"LEAD-TASK", workflow:"wf"}`, later turns `textReply("root done")`. Lead and readers follow the Task 7 script. Assert:
  1. The root's turn-0 request offers `spawn_agent`, and its schema lists `wf`. Extend `gatewayCall` only if needed. Otherwise assert from the raw request body captured in `routedGateway`. Keep the existing struct untouched for the other tests.
  2. Exactly 2 `READ-TASK` requests. Each reader schema is exactly `read_file` plus the five blackboard tools.
  3. The lead's turn-1 tool results: exactly one contains `workflow spawn quota exhausted` (`ErrWorkflowQuotaExhausted` text), and two contain `read done`.
  4. Snapshots (`cap.last()`): node `lead` has `WorkflowRoot == "wf"` and `ParentID == tree.RootID()`. The root and readers have `WorkflowRoot == ""`. `WorkflowRootOf(reader) == lead.ID`. `Scheduler().Pool(lead.ID).Total == 2`.
- `TestHostedWorkflowRefusals`: root turn 0 sends two parallel calls, `spawn_agent{label:"bad", task:"BAD-TASK", workflow:"nope"}` and `spawn_agent{label:"lead", task:"LEAD-TASK", workflow:"wf"}`. Lead turn 0 sends two parallel calls, `spawn_agent{label:"u", task:"U-TASK", worker:"unknown"}` and `spawn_agent{label:"n", task:"N-TASK", workflow:"wf"}`. Then everything replies with text. Assert:
  1. A root turn-1 tool result contains `unknown workflow "nope"`.
  2. Lead turn-1 tool results: one contains `unknown worker "unknown"`, and one contains `nested workflow activation refused`.
  3. No request carries `BAD-TASK`, `U-TASK` or `N-TASK`. There are no tree nodes labelled `bad`, `u` or `n` (refused before node creation).
- `TestHostedPipelineRunWithWorkflowBindsStageWorkers`: root turn 0 sends `pipeline_run{workflow:"wf", definition:"name: p\nsteps:\n  - name: s1\n    worker: reader\n    prompt: STEP-TASK\n"}` (definition as a plain Go string inside the args map), later turns text. `STEP-TASK` replies `textReply("step done")`. Assert:
  1. Exactly one `STEP-TASK` request, whose schema is exactly `read_file` plus the five blackboard tools.
  2. A root turn-1 tool result contains `pipeline p: completed`.
  3. The tree has a node labelled `pipeline p` with `WorkflowRoot == "wf"` and the root as parent. The step node's `WorkflowRootOf` is that node. `Pool(coord.ID).Total == 2`.

Run: `go test ./cmd/fuse -run 'TestHosted' -count=1 -race`. Expect green after Task 6. A red result is a wiring defect in `loop_server.go`. Fix it there.

**Step 3:** Run the existing `TestLoopServer*` harness tests (`go test ./cmd/fuse -run 'TestLoopServer' -count=1`) to confirm the helper split changed nothing.

**Step 4: Commit** `test(fuse): hosted loops enforce spec workflow pools, worker tools and nesting`

---

## Task 9: Final verification

**Tier:** economy

**Steps:**
1. `gofmt -l ./cmd ./internal` prints nothing.
2. `go build ./... && go vet ./...`
3. `go test ./... -count=1 -race`
4. `grep -n 'childBuilder := func' cmd/fuse/*.go` lists exactly the sites the guard counts. `grep -rn 'resolveWorkerTools(' cmd/fuse --include='*.go' | grep -v _test` shows only `workflow.go`.
5. If anything needed a fix, commit `chore(fuse): post-verification fixes for per-spawn workflows`. Otherwise do not create a commit.

## Acceptance coverage map

| Spec acceptance | Test |
|---|---|
| Third spawn refused with `ErrWorkflowQuotaExhausted` (`pool.total: 2`) | Task 8 `TestHostedWorkflowSpawnEnforcesPoolAndWorkerTools` (3), Task 7 local table, Task 4 `TestScopeBackstopEnforcesTotal` |
| Reader schema is exactly `read_file` plus blackboard tools | Task 8 (2), Task 7, Task 5 `TestChildToolsWorkerBoundSchemaIsAllowlistPlusBlackboard` |
| `worker: unknown` errors | Task 8 `TestHostedWorkflowRefusals` (2), Task 4 |
| `workflow` inside an active subtree errors | Task 8 `TestHostedWorkflowRefusals` (2), Task 7 research-probe test, Task 1 |
| Node snapshots show `WorkflowRoot` | Task 8 (4), Task 1 |
| `pipeline_run` with `workflow: wf` binds stage workers | Task 8 `TestHostedPipelineRunWithWorkflowBindsStageWorkers`, Task 5 runner tests |
| Unknown workflow name is a tool error | Task 8 `TestHostedWorkflowRefusals` (1), Task 4 |
| Every binding uses one path; research-probe root activation kept | Task 6 guard and `TestResearchProbeRootActivatesThroughScope`, Task 7 |
| Tool descriptions list workflow names | Tasks 2, 3, 8 (1) |
