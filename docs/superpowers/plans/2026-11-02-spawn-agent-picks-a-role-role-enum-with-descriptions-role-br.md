<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0114 — spawn_agent picks a role: role enum with descriptions, role brief composed into the child prompt](../../changes/active/0114-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br.md)**
<!-- docket:backlink:end -->

# spawn_agent picks a role — Implementation Plan

> **For the executor:** this plan is executed by `docket-build`, one task at a time, each task
> by a tier agent under the `docket-build-task` contract (focused test first, implement,
> verify, self-review, one commit). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `spawn_agent` gains an optional `role` parameter. The runtime builds the child from
the chosen role: its brief goes into the child's system prompt, its tool set (when non-empty)
replaces the child's tools and gets the blackboard tools, and its model pin applies unless the
call names a model. With no roles defined, every schema, prompt and event is byte-identical to
today's.

**Architecture:** Role resolution lives in `workflowScope` (`cmd/fuse/workflow.go`) beside
worker resolution. `checkRequest` refuses bad role requests before any node exists.
`prepareChild` returns a `childPlan` that now carries the role's brief, model and name.
`loopWiring.childTools` returns that plan. Each of the four child-builder closures passes
`plan.instructions` where it passes `""` today. The role name is carried on
`agent.SpawnOpts.Role` and stamped on the tree node, the `spawn.start` payload and the spawn
span.

**Tech Stack:** Go. Tests use `go test`. The repo suite is `make test`, which runs
`observability-validate` and then `go test ./...`.

**Spec:** `docs/superpowers/specs/2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br-design.md`
**Depends on:** 0113 (merged). It added `config.Config.Roles map[string]RoleConfig` and
`WorkflowConfig.Roles []string`. A `RoleConfig` with nil or empty `Tools` inherits the
parent's tools.

---

## Ground truth and decisions made while planning

Read these before starting any task. They settle points the spec leaves open.

1. **Child-builder sites. Grep for them; do not rely on this list** (learning
   `patch-every-cloned-child-builder`). At HEAD `289490c`, `grep -rn "agent.WithChildBuilder("
   cmd/fuse/*.go` finds four closures:
   - `cmd/fuse/runtime_binding.go`: one-shot "1 of 4" (`buildOneShotRuntimeDeps`), shell
     "2 of 4" (`buildShellRuntimeDeps`) and research-probe "3 of 4"
     (`buildResearchProbeRuntimeDeps`);
   - `cmd/fuse/loop_server.go`: "4 of 4". The networked binding (`loop_serve_net.go`) and
     harness-spec loops (`loop_harness.go`) reuse it.

   `cmd/fuse/shell.go:333` is the fifth place the spec names. It is not a separate closure:
   it is the shell entry point that wires site 2, and its comment says "2 of 3" when it should
   say "2 of 4". The parity test has **five rows**: one-shot, shell, research-probe, and
   loop-server twice. One loop-server row takes roles from the operator config and the other
   from a harness spec, which resolves config by a different path. A guard row count keeps
   the table in step with the grep.
2. **"`role: writer` inherits the parent's tools, without the blackboard tools".** On the
   inherit path (empty `role.tools`), `childToolRegistry` clones the parent's registry, which
   already holds the blackboard tools, and `wireChildBlackboard` rebinds them to the child.
   That is today's behaviour for a plain freeform child. The spec's own Blackboard bullet
   says an empty `role.tools` "leaves `bound = false`, which is the unchanged inherit path".
   So the test asserts **writer's tool set == a plain no-role child's tool set under the same
   binding**: nothing is added and nothing is withheld. It does not assert that the blackboard
   tools are absent.
3. **A role that exists but is not offered.** Inside a workflow whose `roles` list is
   non-empty, a defined role missing from that list is refused, because the enum does not
   offer it. The error is
   `role "x" is not available in workflow "wf" (available: a, b)`. The offered set comes from
   the **parent's** governing workflow, since that is the schema the parent saw. So
   `checkRequest` uses `parent.ID` and `prepareChild` uses `child.ParentID`. For a child that
   roots a new workflow, the parent is outside every workflow, so all roles are offered.
4. **`role` together with `workflow` is allowed.** The role becomes the workflow lead's
   definition. The spec does not refuse it.
5. **Refusal texts** (all are tool errors; `SpawnAgentTool.Execute` prefixes `spawn_agent: `):
   - role with system_prompt: `a role supplies the system prompt; drop system_prompt or the role`
   - role with worker: `role %q and worker %q cannot be combined; pass one`
   - unknown role: `unknown role %q (available: <sorted names>|none configured)`
   - widening: `role %q does not permit tool %q`
6. **The role-preference sentence.** It is the exact constant
   `spawnRoleHint = "\n\nPrefer a named role when one fits the task."`. It is appended right
   after `spawnAgentBlock` (which ends in `synthesise.` with no trailing newline) and only
   when `len(cfg.Roles) > 0`. `spawnAgentBlock` itself does not change.
   `withoutUnavailableSpawnBlock` must strip the sentence along with the block.
7. **Span attribute.** Add `{Key: "role", Value: opts.Role}` only when `opts.Role != ""`, so
   spans without a role are unchanged. No metrics label reads spawn fields (Prometheus reads
   only `model`/`tool`), so the closed label allowlist trap does not apply.
8. **Role descriptions in the schema** are collapsed to one line each
   (`strings.Join(strings.Fields(desc), " ")`) so each role is exactly one `- name — description`
   line.
9. **Out of scope (do not touch):** pipeline steps naming a role (`internal/pipeline`), the Go
   `runtime.SpawnOpts`/`inProcRuntime.Spawn` path, worker semantics, the built-in `research`
   workflow, per-role limits, and the proto/TS stubs. The `Role.tools` proto comment already
   promises the blackboard tools; this change makes the runtime keep that promise.
10. **Writing rule.** Code, comments, test names and commit messages describe fuse only. Never
    mention any downstream consumer application.
11. **Live check** uses a cheap gateway model only (`glm` or `kimi` via the gateway at
    `localhost:4000`). Never use an Anthropic model.

## File map

| File | Change |
|---|---|
| `internal/tools/spawn_agent.go` | `SpawnRequest.Role`, `RoleOption`, `WithRoles`, `role` param, input parse |
| `internal/tools/spawn_agent_test.go`, `internal/tools/testdata/spawn_agent_schema*.golden` | schema goldens + role tests |
| `internal/agent/spawn.go` | `SpawnOpts.Role`; node `Role`; span field; spawn.start `Role` |
| `internal/agent/tree.go` | `AgentNode.Role`, `NodeView.Role` |
| `internal/event/event.go` | `SpawnStartPayload.Role` (`json:"role,omitempty"`) |
| `cmd/fuse/workflow.go` | `newWorkflowScope(tree, workflows, roles)`, role checks, `resolveRoleTools`, `childPlan` fields, `spawnTool` roles |
| `cmd/fuse/run.go` | `spawnFuncFrom` maps `Role`; `spawnRoleHint` |
| `cmd/fuse/prompt_blocks.go` | `spawnGuidance(cfg)`; strip the hint too |
| `cmd/fuse/loop_wiring.go` | `childTools` returns `childPlan`; role-bound blackboard |
| `cmd/fuse/runtime_binding.go`, `cmd/fuse/loop_server.go` | four sites: `plan.model`, `plan.instructions` |
| `cmd/fuse/shell.go`, `cmd/fuse/research_probe.go` | stale comment; scope constructor arg |
| `cmd/fuse/role_parity_test.go` (new) | the five-row parity test |
| `internal/config/schema.go`, `README.md` | drop the "runtime does not use roles yet" text |

---

### Task 1: Pin today's spawn_agent schema and spawn.start JSON byte for byte

**Tier:** economy. Tests only, no production change.

**Files:**
- Modify: `internal/tools/spawn_agent_test.go`
- Create: `internal/tools/testdata/spawn_agent_schema.golden`, `internal/tools/testdata/spawn_agent_schema_workflows.golden`, `internal/tools/testdata/spawn_agent_schema_workers.golden`
- Modify: `internal/event/event_test.go`

- [ ] **Step 1: Add the golden test** to `internal/tools/spawn_agent_test.go`:

```go
var updateGolden = flag.Bool("update-golden", false, "rewrite internal/tools golden files")

// schemaGolden renders a tool's model-facing surface (description + parameters)
// as stable, indented JSON. encoding/json sorts map keys, so the bytes are stable.
func schemaGolden(t *testing.T, tool *SpawnAgentTool) []byte {
	t.Helper()
	b, err := json.MarshalIndent(map[string]any{
		"description": tool.Description(),
		"parameters":  tool.Parameters(),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestSpawnAgentSchemaGolden pins the spawn_agent schema and description byte
// for byte for each shape a binding builds without roles: plain, outside a
// workflow (workflow enum), and inside one (worker enum).
func TestSpawnAgentSchemaGolden(t *testing.T) {
	for _, tc := range []struct {
		file string
		tool *SpawnAgentTool
	}{
		{"testdata/spawn_agent_schema.golden", NewSpawnAgentTool(okSpawn("x"))},
		{"testdata/spawn_agent_schema_workflows.golden", NewSpawnAgentTool(okSpawn("x")).WithWorkflows([]string{"wf", "research"})},
		{"testdata/spawn_agent_schema_workers.golden", NewSpawnAgentTool(okSpawn("x")).WithWorkers([]string{"reader"})},
	} {
		t.Run(tc.file, func(t *testing.T) {
			got := schemaGolden(t, tc.tool)
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(tc.file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tc.file, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("spawn_agent schema differs from %s:\n got: %s\nwant: %s", tc.file, got, want)
			}
		})
	}
}
```

Add the imports this needs (`bytes`, `encoding/json`, `flag`, `os`, `path/filepath`).

- [ ] **Step 2: Generate the goldens** from the unchanged code:
  `go test ./internal/tools/ -run TestSpawnAgentSchemaGolden -update-golden`. Then run it
  again without the flag and confirm it PASSES.
- [ ] **Step 3: Pin the spawn.start JSON** in `internal/event/event_test.go`:

```go
// TestSpawnStartPayloadJSONWithoutRole pins the wire bytes of a spawn.start
// payload that names no role: adding the role field must not change them.
func TestSpawnStartPayloadJSONWithoutRole(t *testing.T) {
	b, err := json.Marshal(SpawnStartPayload{ChildNodeID: "n2", Label: "researcher", Task: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"child_node_id":"n2","label":"researcher","task":"do it"}`
	if string(b) != want {
		t.Fatalf("spawn.start JSON = %s, want %s", b, want)
	}
}
```

- [ ] **Step 4: Run** `go test ./internal/tools/ ./internal/event/`. Expected: PASS.
- [ ] **Step 5: Commit** `test: pin the spawn_agent schema and spawn.start JSON byte for byte`.

---

### Task 2: Role on SpawnOpts, the tree node, spawn.start and the spawn span

**Tier:** standard.

**Files:**
- Modify: `internal/agent/spawn.go` (`SpawnOpts`, `Spawn` span, `spawnLocal` node literal, `emitSpawnStart`)
- Modify: `internal/agent/tree.go` (`AgentNode`, `NodeView`, `Snapshot`)
- Modify: `internal/event/event.go` (`SpawnStartPayload`)
- Test: `internal/agent/spawn_event_test.go`, `internal/event/event_test.go`

- [ ] **Step 1: Write the failing tests.**
  - In `internal/agent/spawn_event_test.go`, add `TestSpawnerStampsRole`, modelled on
    `TestSpawnerEmitsSpawnStartDone` (a `recordingStore` plus a stub `WithChildBuilder`).
    Spawn with `SpawnOpts{Label: "rev", Task: "t", Role: "reviewer"}`, wait, then assert:
    - `tree.Node(h.NodeID).Snapshot().Role == "reviewer"`;
    - the decoded `event.SpawnStartPayload.Role == "reviewer"`.

    Also spawn once with no role and assert `Snapshot().Role == ""` and an empty payload
    `Role`.
  - In the same file, add `TestSpawnSpanCarriesRoleOnlyWhenSet`. It needs a small observer
    double in package `agent`: a `Start` that records `observe.Descriptor` and returns a
    handle whose `End` is a no-op. Check first whether `internal/agent` tests already have
    one (`grep -n "observe.Descriptor" internal/agent/*_test.go`) and reuse it if so. Assert:
    - the `OperationSpawn` descriptor for a role spawn has the fields
      `[{worker ""} {role reviewer}]`;
    - a spawn without a role has exactly `[{worker ""}]`.
  - In `internal/event/event_test.go`, add a round trip for
    `SpawnStartPayload{ChildNodeID: "n2", Role: "reviewer"}`. Its JSON must contain
    `"role":"reviewer"`.
- [ ] **Step 2: Run** `go test ./internal/agent/ ./internal/event/`. Expected: FAIL
  (`unknown field Role`).
- [ ] **Step 3: Implement.**
  - `SpawnOpts`: add `Role string` after `Workflow`. Doc comment: "Role names a top-level
    role (change 0114); empty for every other spawn. The binding's child builder resolves it
    to the role's brief, tool set and model."
  - `AgentNode`: add `Role string`, a create-once field like `WorkflowRoot`. Doc comment:
    "the role this node was spawned as, if any (0114)". Add `Role string` to `NodeView` and
    copy it in `Snapshot()`.
  - `spawnLocal` node literal: add `Role: opts.Role`.
  - `Spawn`:

```go
fields := []observe.Field{{Key: "worker", Value: opts.Worker}}
if opts.Role != "" {
	fields = append(fields, observe.Field{Key: "role", Value: opts.Role})
}
ctx, span := s.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationSpawn, Name: "child", Fields: fields})
```

  - `event.SpawnStartPayload`: add `Role string `json:"role,omitempty"``. In
    `emitSpawnStart`, set `Role: node.Role`.
- [ ] **Step 4: Run** `go test ./internal/agent/ ./internal/event/`. Expected: PASS. Task 1's
  JSON pin still passes.
- [ ] **Step 5: Commit** `agent: carry the spawn role on the node, spawn.start and the spawn span`.

---

### Task 3: spawn_agent offers roles: `role` enum with one description line per role

**Tier:** standard.

**Files:**
- Modify: `internal/tools/spawn_agent.go`
- Test: `internal/tools/spawn_agent_test.go`

- [ ] **Step 1: Write the failing tests.**
  - `TestSpawnAgentNoRolesNoParam`: `WithRoles(nil)` leaves every golden from Task 1
    byte-identical. Re-render each golden tool with `.WithRoles(nil)` and compare to the file.
  - `TestSpawnAgentWithRolesAddsEnumAndLines`: call
    `WithRoles([]RoleOption{{"writer","Writes prose."},{"reviewer","Reviews code\nfor bugs."}})`
    and check that:
    - `props["role"]` has `"type":"string"` and `enum == ["reviewer","writer"]` (sorted);
    - its description contains `"\n- reviewer — Reviews code for bugs."` and
      `"\n- writer — Writes prose."`, in that order (the newline is collapsed);
    - `Description()` is unchanged from the no-roles tool.
  - `TestSpawnAgentForwardsRole`: Execute with
    `{"label":"c","task":"t","role":"reviewer"}` captures `SpawnRequest.Role == "reviewer"`.
    Use `captureSpawn`.
- [ ] **Step 2: Run** `go test ./internal/tools/ -run 'Role|Golden'`. Expected: FAIL.
- [ ] **Step 3: Implement.**

```go
// RoleOption is one role the spawn tool offers (change 0114): its name and the
// one-line description the model reads when choosing whom to hand work to.
type RoleOption struct {
	Name        string
	Description string
}
```

  - `SpawnRequest`: add `Role string` (doc: "Role names a top-level role (change 0114).
    Empty for an ordinary spawn.").
  - `SpawnAgentTool`: add `roles []RoleOption`.
  - `WithRoles(opts []RoleOption) *SpawnAgentTool`: copy the slice, sort it by `Name`, and
    collapse each `Description` with `strings.Join(strings.Fields(d), " ")`. An empty or nil
    slice leaves the param absent.
  - In `Parameters()`, after the `workflow` block:

```go
if len(t.roles) > 0 {
	enum := make([]any, len(t.roles))
	var lines strings.Builder
	for i, r := range t.roles {
		enum[i] = r.Name
		lines.WriteString("\n- " + r.Name + " — " + r.Description)
	}
	props["role"] = map[string]any{
		"type": "string",
		"enum": enum,
		"description": "Optional named role for the child. The role supplies the child's system prompt, " +
			"and its tool set and model when it defines them (tools may only narrow a role's tool set; " +
			"model overrides its model). Do not pass system_prompt or worker with a role. Roles:" +
			lines.String(),
	}
}
```

  - `spawnAgentInput`: add `Role string `json:"role"``. In `Execute`, set
    `Role: input.Role` on the request.
- [ ] **Step 4: Run** `go test ./internal/tools/`. Expected: PASS, goldens unchanged.
- [ ] **Step 5: Commit** `tools: spawn_agent offers roles as an enum with one description line each`.

---

### Task 4: workflowScope resolves roles: offered set, refusals, tool narrowing, child plan

**Tier:** premium. Spawn admission and the tool-set authority are both security-relevant.

**Files:**
- Modify: `cmd/fuse/workflow.go`
- Modify: `cmd/fuse/run.go` (`spawnFuncFrom` maps `Role: req.Role`)
- Modify every `newWorkflowScope(` caller. Production: `runtime_binding.go` (3),
  `loop_server.go`, `research_probe.go`. Tests: `instructions_test.go`,
  `loop_wiring_test.go`, `workflow_activation_test.go`, `workflow_scope_test.go`,
  `workflow_wiring_guard_test.go`. Re-grep: `grep -rn "newWorkflowScope(" cmd/fuse`.
- Modify every `checkRequest(` test call in `workflow_scope_test.go`.
- Test: `cmd/fuse/workflow_scope_test.go`

- [ ] **Step 1: Write the failing tests** in `cmd/fuse/workflow_scope_test.go`. Add a helper
  `scopeRoles()` returning:
  - `reviewer`: `{Description: "Reviews code.", Instructions: "REVIEWER-BRIEF", Tools: [read_file, grep], Model: "rmodel"}`;
  - `writer`: `{Description: "Writes prose.", Instructions: "WRITER-BRIEF"}`.

  Build a scope over `scopeWF()` with `wf.Roles = []string{"writer"}`. Tests:
  - `TestCheckRequestRefusesRoleMisuse`, table-driven over
    `checkRequest(root, tools.SpawnRequest{...})`, each expecting an error containing the
    decision-5 text:
    - `role+system_prompt`;
    - `role+worker`;
    - unknown role `ghost` (error lists `reviewer, writer`);
    - widening `role: reviewer, tools: [read_file, bash]`;
    - from a node inside `wf`, `role: reviewer` (not offered: error names workflow `wf` and
      lists `writer`).

    Also assert that `role: reviewer` from root, `role: reviewer, tools: [grep]` from root,
    and `role: writer` inside `wf` all return nil. After every refused case assert
    `len(tree.SnapshotAll())` is unchanged, so no node was created.
  - `TestCheckRequestUnknownRoleWithNoRolesConfigured`: a scope with nil roles refuses
    `role: x` with `none configured`.
  - `TestPrepareChildRolePlan`, through the existing `prepareChild` stub harness:
    - `role: reviewer` gives a plan with `tools [read_file grep]`, `bound true`,
      `model "rmodel"`, `instructions "REVIEWER-BRIEF"` and `role "reviewer"`;
    - `role: reviewer, tools: [grep]` gives `tools [grep]` and `bound true`;
    - `role: writer` gives `tools nil`, `bound false`, `instructions "WRITER-BRIEF"` and
      `model ""`;
    - `role: writer, tools: [read_file]` gives `tools [read_file]` and `bound false`. This
      is the freeform narrowing of the parent's tools.
    - `prepareChild` with `Role: "reviewer", SystemPrompt: "x"` errors. This is defense in
      depth for paths that call `Spawner.Spawn` directly.
  - `TestSpawnToolAdvertisesOfferedRoles`. On the root (outside `wf`),
    `scope.spawnTool(...).Parameters()` has `role.enum == [reviewer writer]`. On a node
    inside `wf`, it has `role.enum == [writer]`. A scope with nil roles has no `role` key.
- [ ] **Step 2: Run** `go test ./cmd/fuse/ -run 'CheckRequest|PrepareChild|SpawnTool'`.
  Expected: FAIL (it does not compile).
- [ ] **Step 3: Implement** in `cmd/fuse/workflow.go`.
  - `workflowScope` gets `roles map[string]config.RoleConfig`.
    `newWorkflowScope(tree, workflows, roles)` stores it. Production callers pass
    `cfg.Roles`; test callers pass `nil` unless they test roles. The new parameter is
    positional on purpose: the compiler then forces every site to choose, so roles cannot
    end up inert at a composition root (learning `security-knob-inert-at-composition-root`).
  - `childPlan` gains `instructions string` (the role brief, "" when none) and `role string`.
    Update the `bound` comment to "a worker or a role with a non-empty tool set was
    selected".
  - Add:

```go
// roleNames returns every defined role name, sorted.
func (s *workflowScope) roleNames() []string { /* sorted keys of s.roles */ }

// offeredRoles returns the role names a spawner at nodeID is offered: the
// governing workflow's roles when it lists any, else every defined role
// (change 0114). Sorted.
func (s *workflowScope) offeredRoles(nodeID string) []string {
	if act, _ := s.activationOf(nodeID); act != nil && len(act.cfg.Roles) > 0 {
		out := make([]string, 0, len(act.cfg.Roles))
		for _, n := range act.cfg.Roles {
			if _, ok := s.roles[n]; ok {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
	return s.roleNames()
}

// checkRole refuses a role request a spawner at parentID may not make, before
// any node exists: a role with system_prompt or worker, an unknown role, or a
// role the parent's workflow does not offer.
func (s *workflowScope) checkRole(parentID, role, systemPrompt, worker string) error { ... }

// resolveRoleTools returns a role child's tool list and whether it is bound
// (change 0114). A non-empty role.Tools is the child's whole tool set and the
// requested subset may only narrow it (bound: the blackboard tools are added).
// An empty role.Tools inherits: the requested subset passes through unchanged,
// exactly as for a freeform spawn (not bound).
func resolveRoleTools(name string, role config.RoleConfig, requested []string) ([]string, bool, error) { ... }

func unknownRoleErr(name string, names []string) error // "unknown role %q (available: %s)", "none configured" when empty
```

  - `checkRequest(parent *agent.AgentNode, req tools.SpawnRequest) error`:
    - when `req.Role != ""`, run `checkRole(parent.ID, req.Role, req.SystemPrompt, req.Worker)`
      and then `resolveRoleTools(req.Role, s.roles[req.Role], req.Tools)`, returning either
      error;
    - then run the existing workflow/worker logic unchanged, reading `req.Workflow` and
      `req.Worker`;
    - in `spawnFunc`, call `s.checkRequest(parent, req)`.

    The existing `act == nil` refusal is for a `worker`. A role passes it, because roles are
    global.
  - `prepareChild`:
    - when `opts.Role != ""`, run `checkRole(child.ParentID, ...)` first, before any
      activation;
    - activate a workflow-root child as today;
    - then, if `opts.Role != ""`, return
      `childPlan{tools: rt, bound: bound, model: role.Model, instructions: role.Instructions, role: opts.Role}`
      from `resolveRoleTools`;
    - otherwise follow the existing path unchanged.
  - `spawnTool`: build the `[]tools.RoleOption` for `s.offeredRoles(node.ID)` (name plus
    `s.roles[name].Description`). Apply `.WithRoles(opts)` in both the inside-workflow branch
    and the outside branch.
  - `cmd/fuse/run.go` `spawnFuncFrom`: add `Role: req.Role` to the `agent.SpawnOpts` literal.
- [ ] **Step 4: Run** `go test ./cmd/fuse/ -run 'CheckRequest|PrepareChild|SpawnTool|Scope|Workflow'`,
  then `go build ./...`. Expected: PASS. The existing worker and workflow tests still pass.
- [ ] **Step 5: Commit** `fuse: resolve spawn roles in the workflow scope and refuse misuse before a node exists`.

---

### Task 5: Child builders apply the role: brief in the instructions slot, model pin, bound blackboard

**Tier:** premium. All four cloned sites change together.

**Files:**
- Modify: `cmd/fuse/loop_wiring.go` (`childTools` returns `childPlan`)
- Modify: `cmd/fuse/runtime_binding.go` (sites 1 of 4, 2 of 4, 3 of 4)
- Modify: `cmd/fuse/loop_server.go` (site 4 of 4)
- Modify: `cmd/fuse/shell.go:333` (comment "2 of 3" becomes "2 of 4")
- Modify: `cmd/fuse/loop_wiring_test.go` (harness `builder` reads `plan.model`)
- Test: `cmd/fuse/loop_wiring_test.go`

- [ ] **Step 1: Write the failing tests** in `cmd/fuse/loop_wiring_test.go`. The harness
  config gets `cfg.Roles = scopeRoles()` (the Task 4 helper) and passes it to
  `newWorkflowScope`. The `built` struct gains `plan childPlan`.
  - `TestRoleChildToolsReviewerBound`: spawn from root with `Role: "reviewer"`. The
    registry names, sorted, are exactly
    `[blackboard_delete blackboard_keys blackboard_read blackboard_wait blackboard_write grep read_file]`.
    There is no `spawn_agent` and no `pipeline_run`. The plan has `model == "rmodel"` and
    `instructions == "REVIEWER-BRIEF"`.
  - `TestRoleChildToolsWriterInherits`: spawn a plain child (no role) and a
    `Role: "writer"` child from root. Their sorted registry names are equal (ground-truth
    decision 2). The writer plan has `instructions == "WRITER-BRIEF"` and `model == ""`.
  - `TestRoleCallModelWins`: in a site-level test or a harness assertion, check that the
    chosen model is `opts.ModelID` when it is set, then `plan.model`, then the binding
    alias. The parity test in Task 7 covers precedence end to end, so here assert only that
    `plan.model` is returned.
- [ ] **Step 2: Run** `go test ./cmd/fuse/ -run 'RoleChild|Wiring'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `loopWiring.childTools(base, opts, childNode) (*tools.Registry, childPlan, error)`
    returns `plan` in place of `plan.model`. Rewrite the doc comment's last sentence: "a
    bound child (a worker, or a role with a non-empty tool set) always gets the five
    blackboard tools, whatever its tool list names." The `bbReq` logic is unchanged
    (`plan.bound` already covers it).
  - At each of the four sites (re-grep `wiring.childTools(`), rename
    `childToolReg, workerModel, terr :=` to `childToolReg, cp, terr :=`. Use `cp.model` where
    `workerModel` was, and keep the comment "worker or role model pin (changes 0034, 0114)".
    In the non-`SystemPrompt` branch, replace the `""` instructions argument with:

```go
cp.instructions /* subagents never get the harness instructions; a role spawn gets its role brief (0114) */
```

    The `opts.SystemPrompt != ""` branch is unchanged. A role with `system_prompt` was
    refused upstream, so a role spawn never reaches `buildChildAgent`.
  - `shell.go:333`: change "(cloned child-builder site 2 of 3)" to
    "(cloned child-builder site 2 of 4)".
- [ ] **Step 4: Run** `go test ./cmd/fuse/`. Expected: PASS. `TestEveryChildBuilderUsesSharedWorkflowWiring`,
  the blackboard and pipeline wiring guards and `TestLocalRootsGetInstructionsChildrenDoNot`
  still pass.
- [ ] **Step 5: Commit** `fuse: child builders apply the spawn role's brief, tools and model at every site`.

---

### Task 6: One role-preference sentence after the spawn block, only when roles exist

**Tier:** economy.

**Files:**
- Modify: `cmd/fuse/run.go` (add `spawnRoleHint` const beside `spawnAgentBlock`; the block itself is untouched)
- Modify: `cmd/fuse/prompt_blocks.go`
- Modify: `cmd/fuse/runtime_binding.go` (research probe: the child extra at the "3 of 4" site and the root at `BuildAgent` use `spawnGuidance(cfg)` in place of the bare `spawnAgentBlock`)
- Test: `cmd/fuse/prompt_blocks_test.go` (create it if absent; check with `ls cmd/fuse/prompt_blocks*`)

- [ ] **Step 1: Write the failing tests.**
  - `TestRootSystemBlockNoRolesUnchanged`: with `cfg.Roles == nil`, `rootSystemBlock(cfg, nil)`
    equals `spawnAgentBlock` exactly.
  - `TestRootSystemBlockRolesAppendsHint`: with one role defined, the block equals
    `spawnAgentBlock + "\n\nPrefer a named role when one fits the task."`, and the sentence
    appears exactly once.
  - `TestRootSystemBlockSpawnDisabledNoHint`: with roles defined and
    `permissions.disabled: [spawn_agent]`, the result is `""`.
  - `TestWithoutUnavailableSpawnBlockStripsHint`: for
    `extra := "X" + spawnAgentBlock + spawnRoleHint + "Y"` and a registry without
    `spawn_agent`, the result is `"XY"`. With `spawn_agent` registered, the result is `extra`
    unchanged.
  - The existing `TestLoopRootPromptGolden` (no roles) must still pass unchanged.
- [ ] **Step 2: Run** `go test ./cmd/fuse/ -run 'SystemBlock|SpawnBlock|Golden'`. Expected: FAIL.
- [ ] **Step 3: Implement.**

```go
// spawnRoleHint is the one sentence added after spawnAgentBlock when roles are
// defined (change 0114). It is a separate string so spawnAgentBlock stays the
// exact constant withoutUnavailableSpawnBlock matches. Roles themselves are
// listed only in the spawn_agent schema, never a second time here.
const spawnRoleHint = "\n\nPrefer a named role when one fits the task."

// spawnGuidance is the parallel-subagents block plus, when cfg defines roles,
// the role-preference sentence.
func spawnGuidance(cfg config.Config) string {
	if len(cfg.Roles) == 0 {
		return spawnAgentBlock
	}
	return spawnAgentBlock + spawnRoleHint
}
```

  - `rootSystemBlock`: write `spawnGuidance(cfg)` in place of `spawnAgentBlock`.
  - `withoutUnavailableSpawnBlock`: when stripping, first
    `strings.ReplaceAll(extra, spawnAgentBlock+spawnRoleHint, "")`, then
    `strings.ReplaceAll(..., spawnAgentBlock, "")`.
  - Research probe (`runtime_binding.go`): the child `buildAgentCore(..., spawnAgentBlock, ...)`
    and the root `buildAgentCore(..., spawnAgentBlock, cfg.Instructions, ...)` both pass
    `spawnGuidance(cfg)`.
- [ ] **Step 4: Run** `go test ./cmd/fuse/`. Expected: PASS, goldens unchanged.
- [ ] **Step 5: Commit** `fuse: add a role-preference sentence after the spawn block when roles exist`.

---

### Task 7: Table-driven parity test across every child-builder site

**Tier:** standard. Tests only. If a row fails, fix the site in this task.

**Files:**
- Create: `cmd/fuse/role_parity_test.go`

The test reuses the existing helpers: `routedGateway`, `toolCallsReply`, `textReply`,
`argsJSON`, `sortedToolNames`, `assertInOrder`, `defaultPersonaPrompt`-style persona lookup,
`runHostedSpec`, `turnRequest` and `nonTestGoFiles`, plus the deps-builder calls exactly as
`TestLocalRootsGetInstructionsChildrenDoNot` in `cmd/fuse/instructions_test.go` makes them.

- [ ] **Step 1: Write the test.**
  - **Operator YAML** (`roleOperatorYAML`): `wfOperatorYAML` plus

```yaml
roles:
  reviewer:
    description: Reviews code for correctness.
    instructions: REVIEWER-BRIEF
    tools: [read_file, grep]
    model: house
  writer:
    description: Writes prose.
    instructions: WRITER-BRIEF
```

  - **Rows** (`name`, plus a `run` func that returns the routed gateway and the tree after
    the root issued its spawns):
    1. `one-shot`: `buildOneShotRuntimeDeps(...)`, then
       `toolReg.Execute(ctx, "spawn_agent", args)` for each child;
    2. `shell`: `buildShellRuntimeDeps(shellDepsInput{...})`, the same way;
    3. `research-probe`: `buildResearchProbeRuntimeDeps(researchProbeDepsInput{..., scope: newWorkflowScope(tree, cfg.Workflows, cfg.Roles)})`,
       the same way;
    4. `loop-server/operator`: `runHostedSpec` over an operator config carrying the roles.
       Use a `harnessServerWith(t, hostedOperatorYAML+roles, raw)` variant. If
       `runHostedSpec` hard-codes `hostedOperatorYAML`, add a sibling
       `runHostedSpecWith(t, operatorYAML, spec, route)` and make `runHostedSpec` call it.
       Have the root script emit the spawns on turn 0;
    5. `loop-server/spec`: `runHostedSpec` with a `*harnessv1.HarnessSpec` whose
       `Roles` map has the same two roles (`harnessv1.Role{Description, Instructions, Tools, Model}`)
       over the plain `hostedOperatorYAML`.
  - **Spawns** (per row, three children with distinct task markers):
    - `{"label":"rev","task":"REV-TASK","role":"reviewer"}`;
    - `{"label":"wri","task":"WRI-TASK","role":"writer"}`;
    - `{"label":"pln","task":"PLN-TASK"}` (the plain baseline);
    - and, for precedence, `{"label":"rv2","task":"RV2-TASK","role":"reviewer","model":"fast"}`.
  - **Assertions per row:**
    - REV-TASK first request:
      - `Messages[0].Role == "system"`;
      - `assertInOrder(sys, PersonaPrompt(persona of "house"), "REVIEWER-BRIEF")`;
      - the request's `Model` is house's id (`cloud/house`);
      - `sortedToolNames == [blackboard_delete blackboard_keys blackboard_read blackboard_wait blackboard_write grep read_file]`.
    - RV2-TASK request `Model == "cloud/fast"` (the call's model wins over the role's).
    - WRI-TASK first request:
      - the system prompt has `WRITER-BRIEF` after the persona;
      - `sortedToolNames` equals PLN-TASK's (ground-truth decision 2).
    - PLN-TASK system prompt contains neither brief.
    - The tree node labelled `rev` has `Role == "reviewer"`, `wri` has `Role == "writer"`,
      and `pln` has `Role == ""`.
  - **Exact composition pin** (one non-table test in the same file,
    `TestRoleBriefCompositionByteForByte`): reuse `instructionsTestBuild` from
    `instructions_test.go` with `instructions = "REVIEWER-BRIEF"` and assert
    `a.SystemPrompt() == agent.ComposeSystemPrompt(persona, prefix, "REVIEWER-BRIEF", extra)`
    on both the gateway and the CLI paths. This is the byte-for-byte pin the spec asks to
    reuse.
  - **Row-count guard:**
    `strings.Count` of `"agent.WithChildBuilder("` across `nonTestGoFiles(t)` must be
    `<= 4`. If it grows, the test fails with "a new child-builder site exists; add a row to
    the role parity table". `len(rows)` must be `== count + 1` (the loop-server site has two
    config sources).
  - **No-roles byte check** (`TestNoRolesSpawnSchemaUnchanged`): on a one-shot deps build
    with no roles, the root registry's `spawn_agent` `Parameters()` has no `role` key, and
    its marshalled JSON equals that of a `tools.NewSpawnAgentToolWithBudget(...)` built with
    the same workflows and no roles.
- [ ] **Step 2: Run** `go test ./cmd/fuse/ -run 'RoleParity|RoleBrief|NoRolesSpawn' -count=1 -race`.
  Expected: PASS. If a row fails, the failure names the site. Fix it there by grep, not
  from this plan's list.
- [ ] **Step 3: Commit** `test: role parity across every child-builder site`.

---

### Task 8: Docs and comments say roles now drive spawns

**Tier:** economy.

**Files:**
- Modify: `internal/config/schema.go` (the `RoleConfig` doc)
- Modify: `README.md` ("The `roles` config key" section)

- [ ] **Step 1: Edit `RoleConfig`'s doc comment.** Remove "The runtime does not use roles
  yet; they are parsed, validated and carried through harness specs." Replace it with: "The
  main agent picks a role through spawn_agent's `role` parameter; the child is built from
  the role's brief, tools and model (change 0114)."
- [ ] **Step 2: Edit `README.md`.** Replace "Defining roles does not change how an agent runs
  yet; this release validates and carries them." with a short paragraph:
  - when roles are defined, `spawn_agent` offers a `role` parameter that lists each role's
    description. Outside a workflow every role is offered; inside one, the workflow's `roles`
    are offered (every role when it lists none);
  - the role's instructions become the helper's brief, in the place the top-level
    `instructions` takes for the main agent;
  - a non-empty `tools` list is the helper's whole tool set plus the blackboard tools, and
    the call's `tools` may only narrow it;
  - the call's `model` wins over the role's;
  - `role` cannot be combined with `system_prompt` or `worker`.
- [ ] **Step 3: Run** `go test ./internal/config/ ./cmd/fuse/ -run 'Doc|Readme|Config' -count=1`
  (plus any README-linked doc test, found with `grep -rln "README.md" --include='*_test.go' .`).
  Expected: PASS.
- [ ] **Step 4: Commit** `docs: roles now shape the helpers spawn_agent starts`.

---

### Task 9: Full suite

**Tier:** economy.

- [ ] **Step 1: Run** `make test` from the feature worktree root. Expected: PASS. If it is
  red, check the verdict against the branch tip in a clean detached worktree before acting
  on it (learning `shared-worktree-red-suite-verify-detached`):
  `git worktree add --detach <tmp> HEAD && (cd <tmp> && make test)`.
- [ ] **Step 2: Run** `go vet ./...`. Expected: clean.
- [ ] No commit unless a fix was needed. A fix gets its own regression test and commit.

---

### Task 10: Live check: a cheap gateway model picks a role by its description

**Tier:** standard. This is verification. No production change unless it finds a defect.

**Constraints:** use only a cheap gateway model, `glm` (default in the home config) or `kimi`,
through the gateway at `http://localhost:4000`. **Never use an Anthropic model.** Never edit
the user's `~/.fuse/config.yml`. Run the binary built from **this feature worktree** (learning
`verify-from-feature-worktree-binary`).

- [ ] **Step 1: Build** with `make build` in the feature worktree, which produces `./fuse`.
- [ ] **Step 2: Make an isolated home.** Roles are honored from `~/.fuse/config.yml` and
  harness specs, not from `.fuse.local.yml`.

```bash
LIVE=$(mktemp -d "${TMPDIR:-/tmp}/fuse-role-live.XXXXXX")
mkdir -p "$LIVE/.fuse"
cp ~/.fuse/config.yml "$LIVE/.fuse/config.yml"
cat >> "$LIVE/.fuse/config.yml" <<'YAML'
roles:
  reviewer:
    description: Reviews Go source for correctness bugs and reports them as a list.
    instructions: You are ROLE-REVIEWER. Read the files you are given and list correctness problems only.
    tools: [read_file, grep]
  writer:
    description: Writes short release notes or prose summaries.
    instructions: You are ROLE-WRITER. Write concise prose.
YAML
```

  First confirm that the copied config's `default:` is `glm` (or pass `--model glm`). Also
  confirm that the gateway URL is `http://localhost:4000`, or export
  `LLM_GATEWAY_URL=http://localhost:4000`. If the copied config already has a top-level
  `roles:` key, merge the two entries into it instead of appending.
- [ ] **Step 3: Run one task that matches the reviewer's description.**

```bash
HOME="$LIVE" ./fuse --model glm --trace "$LIVE/trace.jsonl" \
  "Use a helper agent to review internal/tools/spawn_agent.go for correctness bugs and report what it finds."
```

- [ ] **Step 4: Check the trace.** Expected, in this order:
  - (a) the root's request `tools[]` has `spawn_agent` with a `role` property whose enum is
    `[reviewer, writer]` and whose description lists both lines;
  - (b) the root's response carries a `spawn_agent` tool call with `"role":"reviewer"`;
  - (c) the child's request system message contains `ROLE-REVIEWER`, after the persona
    text;
  - (d) the child's request `tools[]` is exactly `read_file`, `grep` and the five
    `blackboard_*` tools.

  Use `grep -n 'ROLE-REVIEWER\|"role"' "$LIVE/trace.jsonl"`. Streamed tool-call arguments
  can arrive as deltas, so read the assembled arguments rather than one fragment.
- [ ] **Step 5: Record the evidence for the build record:** the command, the model, and the
  trace lines for (a) through (d). If the model does not choose a role, rerun once with `kimi`.
  If neither chooses one, report that as a finding: the prompt and schema text may need
  tuning. It is not a test failure to suppress. Do not loosen any assertion.
- [ ] **Step 6: Clean up** with `rm -rf "$LIVE"`. No commit unless a defect was fixed. A fix
  gets a regression test and its own commit.
