<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0034 — Workflows — skill-bound subagent pools with typed workers and spawn quotas](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0034-workflows.md)**
<!-- docket:backlink:end -->

# Plan — 0034 Workflows: skill-bound subagent pools with typed workers and spawn quotas

> Spec: `docs/superpowers/specs/0034-workflows.md` (on `docket` branch)
> Change: 0034-workflows

## Approach & context

This change layers **workflow-scoped** spawn policy on top of change 0033's already-merged
per-turn stripping machinery. It is built in dependency order: the smallest independently-useful
primitive first (the folded-in tools-subset fix), then the config surface, then the subtree-scoped
pool accounting (concurrent / total / max_depth), then the typed-worker `worker` param, then the
research workflow as the first instance, then end-to-end verification.

Key existing seams (on `origin/main`, present in the feature worktree):

- `internal/agent/tree.go` — `AgentTree` owns the spawn semaphore (`spawnSem`), the global budget
  (`maxSpawns` / `SpawnBudget()`), active-child counts (`ActiveCounts()`), and refcounted
  `YieldSlot`/`UnyieldSlot` (change 0012). `MaxDepth = 5`, `MaxConcurrentSpawns = 16`.
- `internal/agent/strip.go` — `NewStripSpawnPredicate(tree, maxConcurrent)` returns the per-turn
  strip predicate (permanent budget term + reversible active-cap term). `Agent.SetStripSpawn(fn)`
  installs it (`internal/agent/agent.go`).
- `internal/agent/spawn.go` — `Spawner`, `SpawnOpts{Label,Task,SystemPrompt,Tools,ModelID,...}`,
  call-time depth/budget backstops (`ErrMaxDepthExceeded`, `ErrSpawnBudgetExhausted`).
- `internal/tools/spawn_agent.go` — the tool: `Parameters()` JSON schema, `spawnAgentInput`,
  `SpawnFunc`, `BudgetFunc`, `NewSpawnAgentToolWithBudget`.
- `cmd/fuse/run.go:360` — `childToolRegistry(parent, names)`: empty names ⇒ `parent.Clone()`
  (carries spawn_agent); a subset ⇒ exactly the named tools (unknown ⇒ error).
- `cmd/fuse/shell.go` (~L165-183) and `cmd/fuse/research_probe.go` (~L116-124) — the two child
  builders. Both call `childToolRegistry`, then (unless at MaxDepth) **unconditionally**
  `Register` a child-wired spawn tool — the defect this change fixes.
- `internal/config/schema.go` — `Config`, `AgentsConfig{MaxSpawns,MaxConcurrent}`, the `raw*`
  on-disk mirrors + normalization, and the layering/tighten-only merge (ADR-0006).

### Learnings that gate this change

- **slot-cap-yield-while-blocked-on-children** (#12): a bounded pool that counts *alive* holders
  rather than *active* work deadlocks at depth ≥ 2. The workflow `concurrent` pool MUST reuse the
  existing yield/unyield refcounting — it must not introduce a second counter that recreates the
  bug. The pool cap is a *reservation within* the global semaphore, enforced by the **strip
  predicate** (schema absence), not by a second blocking semaphore.
- **verify-tool-loop-at-gateway-seam** (#33): schema/strip changes MUST be verified with the real
  binary against a scripted `LLM_GATEWAY_URL` double that logs each request's `tools[]` — the
  teatest harness fakes the Completer seam and never reaches the `cmd/fuse` spawn wiring.
- **yaml-plain-scalar-colon-space**: free-text config fields with `': '` break `yaml.Unmarshal`;
  keep the new config surface structured (maps/lists), no free-text scalars that could carry colons.
- **verify-from-feature-worktree-binary**: build/run the binary from this worktree during
  verification, not the main checkout.

### Design decisions to surface for ADRs (step 6)

- **Pool as strip-predicate reservation, not a second semaphore** — enforcing `concurrent` by
  extending the per-turn strip predicate (subtree-scoped) rather than a blocking semaphore keeps
  the "cap not guarantee" v1 semantics and avoids the depth-2 deadlock. Likely an ADR.
- **Worker allowlist as the `tools`-subset primitive** — typed workers compile down to the
  folded-in tools-subset fix; `worker` is sugar over an allowlist. Confirm during build whether
  this warrants an ADR or is adequately covered by the spec.

## Automated-test strategy

- Unit tests in `internal/agent` for the subtree strip predicate (pool concurrent/total/depth) and
  in `internal/config` for `workflows:` parse + layering (frontmatter default < config;
  local tightens only).
- Unit tests in `internal/tools` for the `worker` param schema + narrowing.
- Builder-level tests (or a focused helper test) for the folded-in tools-subset fix in both
  builders — a child requested without `spawn_agent` has no spawn tool.
- End-to-end gateway-seam verification (manual, documented in results) — the acceptance rig from
  the spec, replaying the deepseek-flash fan-out under the research workflow.

---

## Task 1 — Folded-in fix: honor the tools subset in both child builders

**Goal:** a parent that passes a `tools` subset omitting `spawn_agent` produces a child whose
registry has no spawn tool (spec Acceptance 3). This is the enforcement primitive worker allowlists
compile down to, and is independently useful for freeform spawns.

- **Test first:** add a test proving the registration rule. Prefer a small, extractable helper —
  introduce `func shouldWireChildSpawn(requested []string) bool` (empty ⇒ true; else true iff it
  contains `"spawn_agent"`) in `cmd/fuse` (e.g. `run.go` beside `childToolRegistry`) and unit-test
  it directly (empty slice, `["read_file"]`, `["read_file","spawn_agent"]`). If package-level
  builder tests already exist, add a registry-contents assertion there too.
- **Implement:** in both `cmd/fuse/shell.go` (~L177) and `cmd/fuse/research_probe.go` (~L124),
  guard the `childToolReg.Register(tools.NewSpawnAgentToolWithBudget(...))` call with
  `shouldWireChildSpawn(opts.Tools)`. The MaxDepth `Unregister` branch is unchanged (depth strip
  still wins). When the subset omits spawn_agent, `childToolRegistry` already excluded it, so the
  child simply has no spawn tool — no Unregister needed, but a defensive `Unregister("spawn_agent")`
  in the not-wired path is harmless and explicit.
- **Verify:** `go test ./cmd/... ./internal/agent/...`; `go build ./...`.

## Task 2 — Config surface: `workflows:` and `workers:` schema + layering

**Goal:** `workflows:` parses and layers (frontmatter default < config; `.fuse.local.yml` tightens
only), and a config with no `workflows:` behaves exactly as before (spec Acceptance 6).

- **Test first:** in `internal/config`, add table tests: (a) a `workflows:` block round-trips to a
  typed `WorkflowsConfig`; (b) config-level overrides a frontmatter/embedded default for the same
  workflow name; (c) `.fuse.local.yml` may only *tighten* pool numbers (lower `concurrent`/`total`/
  `max_depth`) and a loosening local value is clamped/rejected per the ADR-0006 trust boundary,
  matching how existing agents/permissions tighten-only merges behave; (d) absent `workflows:` ⇒
  zero-value, no behavior change.
- **Implement:** add to `internal/config/schema.go`:
  - `type WorkerConfig struct { Tools []string `yaml:"tools"`; Model string `yaml:"model"` }`
  - `type PoolConfig struct { Concurrent int `yaml:"concurrent"`; Total int `yaml:"total"`; MaxDepth int `yaml:"max_depth"` }`
  - `type WorkflowConfig struct { Skill string `yaml:"skill"`; Pool PoolConfig `yaml:"pool"`; Workers map[string]WorkerConfig `yaml:"workers"` }`
  - `Workflows map[string]WorkflowConfig `yaml:"workflows"`` on `Config` + its `raw*` mirror +
    normalization, following the existing `AgentsConfig` pattern exactly.
  - Extend the layering/merge so workflow pool numbers follow the same tighten-only direction as the
    rest of fuse config; reuse existing merge helpers rather than adding a parallel path.
  - Keep all fields structured — **no free-text scalars** (learnings: yaml-plain-scalar-colon-space).
- **Verify:** `go test ./internal/config/...`; `go build ./...`.

## Task 3 — Workflow root tagging + subtree scope

**Goal:** the runtime can identify the workflow root and the subtree it governs, so pool accounting
and strips apply only within it (spec: activation; Acceptance 4 — a sibling non-workflow agent keeps
the tool).

- **Test first:** `internal/agent` tests that, given a tree with a tagged workflow-root node, a
  helper reports (a) whether a given node is in that root's subtree, and (b) the subtree's active
  child count and lifetime spawn count scoped to that subtree.
- **Implement:** on `AgentTree`/`AgentNode`, add a minimal workflow-root marker (e.g.
  `AgentNode.WorkflowRoot string` naming the workflow, set when a workflow-bound skill activates)
  and subtree-scoped accessors: `SubtreeActiveCounts(rootID)` (running+pending under rootID) and
  `SubtreeSpawnCount(rootID)` (nodes created under rootID). Reuse the existing DFS/lock patterns in
  `SnapshotAll`. Do **not** add a second semaphore — accounting only.
- **Verify:** `go test ./internal/agent/...`.

## Task 4 — Scoped pool enforcement via a workflow strip predicate

**Goal:** within a workflow subtree, `spawn_agent` is stripped when the pool's `concurrent`
(reversible), `total` (permanent), or `max_depth` (static) limit is reached — schema absence, not
errors, is the steady state (spec Acceptance 1). Global brakes remain the outer bound.

- **Test first:** `internal/agent` table tests for a new `NewWorkflowStripPredicate(tree, rootID,
  pool)`:
  - concurrent: strips while `SubtreeActiveCounts >= pool.Concurrent`, reappears as children exit
    (reversible);
  - total: strips permanently once `SubtreeSpawnCount >= pool.Total`;
  - max_depth: a node at `rootDepth + pool.MaxDepth` never gets the tool (static);
  - the predicate still ORs with the **global** predicate so the tighter of the two governs;
  - a `pool` field of 0 means "unset for that dimension" (no strip on it), matching how
    `SpawnBudget` treats `max==0` and `NewStripSpawnPredicate` treats `maxConcurrent<=0`.
- **Implement:** add `internal/agent/strip.go` `NewWorkflowStripPredicate`, composed with the global
  one (a small `func orPredicates(...) func() bool`). Follow the reversible/permanent asymmetry that
  `NewStripSpawnPredicate` documents (count running+pending for the reversible term). The budget
  line injected into spawn results must report the **tighter** of workflow-total-remaining and
  global-budget-remaining (spec Acceptance 5) — extend `BudgetFunc`/the budget-line source so a
  workflow child reads the binding constraint; add a unit test asserting the tighter value is shown.
- **Wire (in `cmd/fuse`):** when a child is inside a workflow subtree, install
  `orPredicates(NewStripSpawnPredicate(...), NewWorkflowStripPredicate(...))` via
  `a.SetStripSpawn(...)` instead of the global-only predicate. Keep non-workflow sessions on the
  existing global-only predicate (Acceptance 4).
- **Verify:** `go test ./internal/agent/... ./cmd/...`.

## Task 5 — Typed workers: the `worker` spawn param

**Goal:** inside a workflow subtree, `spawn_agent` gains an optional `worker` param enumerating the
workflow's worker names; when given, the child registry is the worker's allowlist exactly, `tools`
may only narrow it, and a worker without `spawn_agent` cannot nest (spec Acceptance 2).

- **Test first:** `internal/tools` tests that `Parameters()` includes a `worker` enum when the tool
  is constructed with a worker-name set, and that the parsed `spawnAgentInput` carries `Worker`.
  `cmd/fuse` (or builder-helper) test: a `worker` naming `facet-researcher` yields a child registry
  equal to that worker's allowlist; a `tools` subset narrows it further; a worker omitting
  spawn_agent yields a child with no spawn tool.
- **Implement:**
  - `internal/tools/spawn_agent.go`: add `Worker string `json:"worker"`` to `spawnAgentInput`;
    thread a `workerNames []string` (and their allowlists) into the tool so `Parameters()` can emit
    the enum and `Execute` can pass `worker` through `SpawnFunc`/`SpawnOpts`. Add
    `SpawnOpts.Worker` (`internal/agent/spawn.go`) and widen `SpawnFunc` accordingly (update all
    call sites).
  - Child builders: when `opts.Worker != ""`, resolve the worker's allowlist from the active
    workflow config, intersect with any `opts.Tools` narrowing, and build the child registry from
    that — reusing Task 1's `shouldWireChildSpawn` over the *resolved* allowlist so a worker without
    spawn_agent structurally cannot nest.
  - A workflow with no `workers:` block ⇒ freeform spawns (no `worker` enum), still pool-bound.
- **Verify:** `go test ./internal/tools/... ./internal/agent/... ./cmd/...`; `go build ./...`.

## Task 6 — Research workflow (first instance) + skill text

**Goal:** `/research` ships as an embedded workflow — `facet-researcher` worker
(`web_search, web_fetch, read_file`), pool `{concurrent: 5, total: 8, max_depth: 1}` — and the
skill text swaps unenforceable prose for worker-typed spawns (spec: Research workflow).

- **Test first:** a test asserting the embedded research skill's frontmatter carries the workflow
  default block and it parses into the expected `WorkflowConfig` (skill=research, the pool numbers,
  the `facet-researcher` allowlist without spawn_agent).
- **Implement:**
  - Embed the workflow default in the research skill frontmatter
    (`internal/skills/embedded/research.md`) per the config layering (frontmatter default < config).
  - Replace the unenforceable prose rules ("depth-1", "4-5 children", "children MUST NOT call
    spawn_agent") with "spawn one `facet-researcher` per facet" and add the 0033-era fallback:
    "if spawn_agent is not among your tools, do the facet work directly."
  - Bind activation so firing `/research` (slash command or skill tool) tags the invoking agent as
    the workflow root (Task 3's marker) and applies the pool for the run.
- **Verify:** `go test ./...`; `go build ./...`.

## Task 7 — Full-suite gate + end-to-end gateway-seam verification

**Goal:** the whole suite is green and the acceptance scenario is verified against the real binary
(spec Acceptance 1-6; learnings: verify-tool-loop-at-gateway-seam, verify-from-feature-worktree-binary).

- Run `go test ./...` and `go build ./...` from the feature worktree.
- Build the acceptance rig from the spec: a scripted `LLM_GATEWAY_URL` double that logs each
  request's `tools[]`, driving the shipped binary (built from **this worktree**) with a
  `.fuse.local.yml` making the pool caps reachable; replay the deepseek-flash fan-out and confirm:
  (1) depth never exceeds 1, total never exceeds 8, concurrent never exceeds 5, with zero refused
  spawn calls once stripping engages (schema absence is steady state); (2) a `facet-researcher`
  child's request never lists `spawn_agent`; (4) a sibling non-workflow agent keeps the tool;
  (5) the budget line shows the tighter remaining.
- Capture the FULL pane before asserting any UI element absent (learnings note). Record the rig,
  commands, and observations in the results file (Step 6.5) for the human to re-run at the merge gate.

## Notes / risks

- Do not re-introduce the depth-2 deadlock: the `concurrent` pool is enforced by *stripping*
  (schema absence) within the subtree, riding the existing yield/unyield semaphore — never a second
  blocking semaphore that charges blocked parents.
- `SpawnFunc`/`SpawnOpts` widening (worker param, tighter budget) touches all call sites — grep for
  every `makeSpawnFunc`/`NewSpawnAgentToolWithBudget`/`WithChildBuilder` before landing Task 5.
- Keep non-workflow sessions byte-for-byte unchanged (Acceptance 6 / 4) — the workflow predicate and
  worker enum only exist inside a tagged subtree.
