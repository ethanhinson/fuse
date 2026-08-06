<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0033 — Strip spawn_agent from tool schemas at the concurrency cap and on budget exhaustion](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0033-spawn-tool-stripping.md)**
<!-- docket:backlink:end -->

# Spawn Tool Stripping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Omit `spawn_agent` from an agent's per-turn tool schemas when the active-child cap is reached (reversible) or the lifetime spawn budget is exhausted (permanent), and never register it for a child created at MaxDepth.

**Architecture:** The agent loop rebuilds tool schemas every turn (`internal/agent/loop.go` calls `a.tools.Schemas()` fresh per inference request). We add an optional per-turn predicate `stripSpawn func() bool` on the `Agent` struct; when it returns true the loop filters the `spawn_agent` schema out of that turn's request. The predicate consults the shared `*AgentTree` via its already-locked `ActiveCounts()` and `SpawnBudget()` snapshots, so stripping and restoration are pure conditional schema assembly with no new caching or locking. The concurrency cap becomes configurable (`agents.max_concurrent`, default 16); depth stripping is done statically at child-registry construction in `cmd/fuse`.

**Tech Stack:** Go. Standard library `testing` (table-driven where natural), existing `internal/agent`, `internal/tools`, `internal/config`, `cmd/fuse` packages.

## Global Constraints

- Defaults after this change: `agents.max_concurrent` = 16, `agents.max_spawns` = 64. Both overridable in config. Existing configs with an explicit `max_spawns` keep their value.
- `MaxDepth` stays a hard const = 5 (`internal/agent/tree.go:13`); it is NOT made configurable.
- The semaphore yield mechanics (`YieldSlot`/`UnyieldSlot`, root exempt via `node.Depth == 0`), the pending-queue semantics, cancellation, and the permission gate MUST remain behaviorally unchanged. Only the semaphore's *size* becomes configurable.
- The call-time backstops `ErrMaxDepthExceeded` (`spawn.go:12`, checked `:86`) and `ErrSpawnBudgetExhausted` (`spawn.go:18`, checked `:93`) MUST remain and still fire for races / hallucinated calls.
- Budget-line injection on successful spawn results (`internal/tools/spawn_agent.go:115` via `budgetLine()`) MUST remain.
- No injected notice when the tool disappears (silent strip).
- The strip predicate MUST be race-safe (it reads only tree methods that lock internally) and MUST NOT cache its result across turns — it is evaluated once per inference request.
- `max_spawns: 0` (unset) means no lifetime budget and no permanent strip, exactly as today.

---

## File Structure

- `internal/config/schema.go` — add `MaxConcurrent` to `AgentsConfig` + `rawAgentsConfig`; raise `MaxSpawns` default 16→64; add `MaxConcurrent` default 16 in `Default()`.
- `internal/config/loader.go` — merge `raw.Agents.MaxConcurrent` (nonzero override) into `c.Agents.MaxConcurrent`.
- `internal/agent/tree.go` — keep `MaxConcurrentSpawns` const as the *default*; add a `NewAgentTreeWithConcurrency(rootLabel, rootModel string, maxConcurrent int)` constructor (or a functional option) sizing `spawnSem`; `NewAgentTree` delegates with the default. No change to yield mechanics.
- `internal/agent/agent.go` — add `stripSpawn func() bool` field to `Agent` and a setter `SetStripSpawn`.
- `internal/agent/loop.go` — filter `spawn_agent` out of the per-turn `Tools` when `a.stripSpawn != nil && a.stripSpawn()`.
- `internal/agent/strip.go` (new) — `NewStripSpawnPredicate(tree *AgentTree, maxConcurrent int) func() bool`: returns true when `used >= max` (max>0) OR `running+pending >= maxConcurrent`.
- `cmd/fuse/run.go` — wire the strip predicate onto root and child agents (in `buildAgentCore` / `buildChildAgent`, or at their call sites in `shell.go`); size the tree via the new constructor.
- `cmd/fuse/shell.go` — build the tree with the configured concurrency; at child spawn (`:159`–`:164`) skip registering `spawn_agent` when `childNode.Depth == MaxDepth`; pass the concurrency to the predicate.
- Tests: `internal/config/loader_test.go`, `internal/agent/strip_test.go` (new), `internal/agent/loop_test.go`, `internal/agent/tree_test.go`, and a regression test in `internal/agent/strip_test.go`.

---

## Task 1: Config plumbing for `agents.max_concurrent` and the raised `max_spawns` default

**Files:**
- Modify: `internal/config/schema.go` (`AgentsConfig` ~:115-117, `rawAgentsConfig` ~:157-159, `Default()` ~:202-204)
- Modify: `internal/config/loader.go` (~:241-245)
- Test: `internal/config/loader_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `config.AgentsConfig{ MaxSpawns int; MaxConcurrent int }`; `config.Default().Agents == AgentsConfig{MaxSpawns: 64, MaxConcurrent: 16}`. Loader merges nonzero overrides for both keys.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/loader_test.go`:

```go
func TestDefaultAgentsConfig(t *testing.T) {
	d := Default()
	if d.Agents.MaxSpawns != 64 {
		t.Errorf("default MaxSpawns = %d, want 64", d.Agents.MaxSpawns)
	}
	if d.Agents.MaxConcurrent != 16 {
		t.Errorf("default MaxConcurrent = %d, want 16", d.Agents.MaxConcurrent)
	}
}

func TestLoadAgentsOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("agents:\n  max_spawns: 32\n  max_concurrent: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if err := loadFile(path, &c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.MaxSpawns != 32 {
		t.Errorf("MaxSpawns = %d, want 32", c.Agents.MaxSpawns)
	}
	if c.Agents.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", c.Agents.MaxConcurrent)
	}
}

func TestLoadAgentsOmittedKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	// max_spawns present but max_concurrent omitted, and vice-versa handled by defaults.
	if err := os.WriteFile(path, []byte("agents:\n  max_spawns: 16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if err := loadFile(path, &c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.MaxSpawns != 16 {
		t.Errorf("explicit MaxSpawns = %d, want 16 (kept)", c.Agents.MaxSpawns)
	}
	if c.Agents.MaxConcurrent != 16 {
		t.Errorf("omitted MaxConcurrent = %d, want 16 (default)", c.Agents.MaxConcurrent)
	}
}
```

Note: confirm the exact loader entry point name and imports used by existing tests in `internal/config/loader_test.go` (it may be `Load`, `loadFile`, or similar) and match it; adjust the helper call and add `os`/`path/filepath` imports if not already present. If the existing tests build config via a different helper, mirror that helper here rather than inventing `loadFile`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestDefaultAgentsConfig|TestLoadAgents' -v`
Expected: FAIL — `MaxConcurrent` undefined and default `MaxSpawns` is still 16.

- [ ] **Step 3: Implement**

In `internal/config/schema.go`, extend `AgentsConfig`:

```go
type AgentsConfig struct {
	MaxSpawns     int `yaml:"max_spawns"`
	MaxConcurrent int `yaml:"max_concurrent"`
}
```

Extend `rawAgentsConfig`:

```go
type rawAgentsConfig struct {
	MaxSpawns     int `yaml:"max_spawns"`
	MaxConcurrent int `yaml:"max_concurrent"`
}
```

In `Default()`, change the `Agents` literal to:

```go
Agents: AgentsConfig{
	MaxSpawns:     64,
	MaxConcurrent: 16,
},
```

In `internal/config/loader.go`, next to the existing `max_spawns` merge (~:241-245), add:

```go
if raw.Agents.MaxSpawns != 0 {
	c.Agents.MaxSpawns = raw.Agents.MaxSpawns
}
if raw.Agents.MaxConcurrent != 0 {
	c.Agents.MaxConcurrent = raw.Agents.MaxConcurrent
}
```

Update the AgentsConfig doc comment to describe `MaxConcurrent` (semaphore bound on concurrently running children; default for the strip cap).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestDefaultAgentsConfig|TestLoadAgents' -v`
Expected: PASS

- [ ] **Step 5: Full package build + test**

Run: `go build ./... && go test ./internal/config/`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/config/schema.go internal/config/loader.go internal/config/loader_test.go
git commit -m "feat(config): add agents.max_concurrent, raise max_spawns default to 64"
```

---

## Task 2: Configurable semaphore size on the tree

**Files:**
- Modify: `internal/agent/tree.go` (`MaxConcurrentSpawns` const ~:15-19, `NewAgentTree` ~:233-251)
- Test: `internal/agent/tree_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `agent.NewAgentTreeWithConcurrency(rootLabel, rootModel string, maxConcurrent int) *AgentTree` — sizes `spawnSem` to `maxConcurrent`; a value <= 0 falls back to `MaxConcurrentSpawns`. `agent.NewAgentTree(rootLabel, rootModel string) *AgentTree` keeps its signature and delegates with `MaxConcurrentSpawns`. `MaxConcurrentSpawns` const becomes 16.

- [ ] **Step 1: Write the failing test**

Add to `internal/agent/tree_test.go`:

```go
func TestMaxConcurrentSpawnsDefaultIs16(t *testing.T) {
	if MaxConcurrentSpawns != 16 {
		t.Fatalf("MaxConcurrentSpawns = %d, want 16", MaxConcurrentSpawns)
	}
}

func TestNewAgentTreeWithConcurrencySizesSemaphore(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 3)
	if cap(tr.spawnSem) != 3 {
		t.Fatalf("spawnSem cap = %d, want 3", cap(tr.spawnSem))
	}
}

func TestNewAgentTreeWithConcurrencyFallsBackOnZero(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 0)
	if cap(tr.spawnSem) != MaxConcurrentSpawns {
		t.Fatalf("spawnSem cap = %d, want %d", cap(tr.spawnSem), MaxConcurrentSpawns)
	}
}

func TestNewAgentTreeUsesDefaultConcurrency(t *testing.T) {
	tr := NewAgentTree("root", "m")
	if cap(tr.spawnSem) != MaxConcurrentSpawns {
		t.Fatalf("spawnSem cap = %d, want %d", cap(tr.spawnSem), MaxConcurrentSpawns)
	}
}
```

(These are white-box tests in package `agent`, so `tr.spawnSem` is accessible. Confirm `tree_test.go` is `package agent`; it is.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run 'MaxConcurrentSpawns|NewAgentTree' -v`
Expected: FAIL — `NewAgentTreeWithConcurrency` undefined; `MaxConcurrentSpawns` is still 8.

- [ ] **Step 3: Implement**

In `internal/agent/tree.go`, change the const value (keep the doc comment, update the number):

```go
// MaxConcurrentSpawns is the DEFAULT width cap on concurrently RUNNING local
// child agents across the whole tree, used when config does not set
// agents.max_concurrent. ...
const MaxConcurrentSpawns = 16
```

Replace `NewAgentTree` with a delegating pair:

```go
// NewAgentTree creates a tree using the default concurrency cap.
func NewAgentTree(rootLabel, rootModel string) *AgentTree {
	return NewAgentTreeWithConcurrency(rootLabel, rootModel, MaxConcurrentSpawns)
}

// NewAgentTreeWithConcurrency creates a tree whose spawn semaphore is sized to
// maxConcurrent (the number of children that may run at once). A value <= 0
// falls back to MaxConcurrentSpawns. Yield/unyield mechanics are unaffected —
// only the semaphore's capacity changes.
func NewAgentTreeWithConcurrency(rootLabel, rootModel string, maxConcurrent int) *AgentTree {
	if maxConcurrent <= 0 {
		maxConcurrent = MaxConcurrentSpawns
	}
	root := &AgentNode{
		ID:     newNodeID(),
		Label:  rootLabel,
		Model:  rootModel,
		Status: StatusRunning,
	}
	return &AgentTree{
		nodes:    map[string]*AgentNode{root.ID: root},
		rootID:   root.ID,
		out:      make(chan TreeUpdate, 256),
		dirty:    map[string]bool{},
		spawnSem: make(chan struct{}, maxConcurrent),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run 'MaxConcurrentSpawns|NewAgentTree' -v`
Expected: PASS

- [ ] **Step 5: Full agent-package test (guards yield mechanics)**

Run: `go build ./... && go test ./internal/agent/`
Expected: PASS — existing spawn/yield tests still pass, confirming the semaphore change did not disturb yield behavior.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/tree.go internal/agent/tree_test.go
git commit -m "feat(agent): configurable spawn-semaphore size; default cap 16"
```

---

## Task 3: The strip predicate

**Files:**
- Create: `internal/agent/strip.go`
- Test: `internal/agent/strip_test.go` (new)

**Interfaces:**
- Consumes: `AgentTree.ActiveCounts() (running, pending int)` (`tree.go:339`); `AgentTree.SpawnBudget() (used, max int)` (`tree.go:223`).
- Produces: `agent.NewStripSpawnPredicate(tree *AgentTree, maxConcurrent int) func() bool` — returns a closure that reports whether `spawn_agent` must be stripped this turn. True when the lifetime budget is exhausted (`max > 0 && used >= max`) OR the active cap is reached (`running+pending >= maxConcurrent`, when `maxConcurrent > 0`). A nil tree yields a predicate that always returns false.

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/strip_test.go`:

```go
package agent

import "testing"

func TestStripPredicateNilTreeNeverStrips(t *testing.T) {
	p := NewStripSpawnPredicate(nil, 16)
	if p() {
		t.Fatal("nil-tree predicate should never strip")
	}
}

func TestStripPredicateBudgetExhausted(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(2)
	p := NewStripSpawnPredicate(tr, 16)
	if p() {
		t.Fatal("with 0/2 used, should not strip")
	}
	// Add two child nodes to reach used == max (root excluded from used).
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	if !p() {
		if used, max := tr.SpawnBudget(); true {
			t.Fatalf("with %d/%d used, should strip (permanent)", used, max)
		}
	}
}

func TestStripPredicateBudgetZeroNeverStrips(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	// max_spawns 0 => no budget strip regardless of node count.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	p := NewStripSpawnPredicate(tr, 100) // high cap so only budget could trigger
	if p() {
		t.Fatal("max_spawns 0 must never strip via budget")
	}
}

func TestStripPredicateActiveCapReached(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	p := NewStripSpawnPredicate(tr, 2)
	if p() {
		t.Fatal("no active children, cap 2: should not strip")
	}
	// Two running children => running+pending == cap.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	if !p() {
		t.Fatal("running+pending >= cap: should strip (reversible)")
	}
}

func TestStripPredicatePendingCountsTowardCap(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	p := NewStripSpawnPredicate(tr, 2)
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusPending})
	if !p() {
		t.Fatal("1 running + 1 pending == cap 2: should strip")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestStripPredicate -v`
Expected: FAIL — `NewStripSpawnPredicate` undefined.

- [ ] **Step 3: Implement**

Create `internal/agent/strip.go`:

```go
package agent

// NewStripSpawnPredicate returns a per-turn predicate deciding whether the
// spawn_agent tool must be omitted from an agent's tool schemas this turn.
//
// It strips when either brake is engaged:
//
//   - Lifetime budget (permanent): the tree-global spawn budget is exhausted,
//     i.e. max > 0 && used >= max. Once true it stays true for the session,
//     because the tree is append-only.
//   - Active-child cap (reversible): the tree's active child count
//     (running + pending) is at or above maxConcurrent. As children finish and
//     the count drops below the cap, this term becomes false again and the tool
//     reappears on the next turn.
//
// The predicate reads only AgentTree methods that lock internally, so it is
// race-safe, and it recomputes on every call — it MUST NOT be cached across
// turns. A nil tree yields a predicate that never strips.
func NewStripSpawnPredicate(tree *AgentTree, maxConcurrent int) func() bool {
	return func() bool {
		if tree == nil {
			return false
		}
		if used, max := tree.SpawnBudget(); max > 0 && used >= max {
			return true
		}
		if maxConcurrent > 0 {
			running, pending := tree.ActiveCounts()
			if running+pending >= maxConcurrent {
				return true
			}
		}
		return false
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run TestStripPredicate -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/strip.go internal/agent/strip_test.go
git commit -m "feat(agent): strip-spawn predicate (budget + active-cap brakes)"
```

---

## Task 4: Loop filters `spawn_agent` from per-turn schemas when the predicate trips

**Files:**
- Modify: `internal/agent/agent.go` (`Agent` struct ~:31-54; add setter)
- Modify: `internal/agent/loop.go` (build of `req.Tools` ~:156-161)
- Test: `internal/agent/loop_test.go`

**Interfaces:**
- Consumes: `Agent.stripSpawn func() bool`; `NewStripSpawnPredicate` from Task 3 (used by callers, not directly here).
- Produces: `(*Agent).SetStripSpawn(fn func() bool)` — installs the per-turn predicate. When set and returning true, the schema named `spawn_agent` is removed from that turn's `CompletionReq.Tools`. When nil (default) behavior is unchanged.

- [ ] **Step 1: Write the failing tests**

The existing `fakeExec` returns `nil` schemas. Add a schema-returning fake and two tests to `internal/agent/loop_test.go`:

```go
// schemaExec returns a fixed schema list including spawn_agent and records the
// schemas presented on the most recent Complete call (captured via the
// completer below).
type schemaExec struct{ fakeExec }

func (s *schemaExec) Schemas() []model.ToolSchema {
	return []model.ToolSchema{
		{Name: "bash"},
		{Name: "spawn_agent"},
		{Name: "read_file"},
	}
}

// capturingCompleter records the tool schemas of the first request, then stops.
type capturingCompleter struct {
	gotTools []model.ToolSchema
}

func (c *capturingCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.gotTools = req.Tools
	return model.CompletionResp{Content: "done"}, nil
}

func toolNames(ts []model.ToolSchema) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func containsName(ts []model.ToolSchema, name string) bool {
	for _, t := range ts {
		if t.Name == name {
			return true
		}
	}
	return false
}

func TestRunKeepsSpawnAgentWhenNoStripPredicate(t *testing.T) {
	comp := &capturingCompleter{}
	a := New(comp, &schemaExec{}, nopRenderer{}, "m", "", 10, 100)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if !containsName(comp.gotTools, "spawn_agent") {
		t.Fatalf("spawn_agent should be present; got %v", toolNames(comp.gotTools))
	}
}

func TestRunStripsSpawnAgentWhenPredicateTrue(t *testing.T) {
	comp := &capturingCompleter{}
	a := New(comp, &schemaExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetStripSpawn(func() bool { return true })
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if containsName(comp.gotTools, "spawn_agent") {
		t.Fatalf("spawn_agent should be stripped; got %v", toolNames(comp.gotTools))
	}
	// The other tools must survive the filter.
	if !containsName(comp.gotTools, "bash") || !containsName(comp.gotTools, "read_file") {
		t.Fatalf("non-spawn tools must remain; got %v", toolNames(comp.gotTools))
	}
}

func TestRunKeepsSpawnAgentWhenPredicateFalse(t *testing.T) {
	comp := &capturingCompleter{}
	a := New(comp, &schemaExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetStripSpawn(func() bool { return false })
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if !containsName(comp.gotTools, "spawn_agent") {
		t.Fatalf("predicate false: spawn_agent should remain; got %v", toolNames(comp.gotTools))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run 'TestRun.*SpawnAgent' -v`
Expected: FAIL — `SetStripSpawn` undefined; and without filtering, the strip test would fail once the method exists.

- [ ] **Step 3: Implement**

In `internal/agent/agent.go`, add the field to `Agent` (after `LoopApproval`):

```go
	// stripSpawn, when set, is consulted once per inference request. When it
	// returns true, the spawn_agent schema is omitted from that turn's tool
	// list (active-cap or budget brake). It must be race-safe and must not be
	// cached across turns. See change 0033.
	stripSpawn func() bool
```

Add a setter:

```go
// SetStripSpawn installs the per-turn spawn-strip predicate. Nil (default)
// leaves spawn_agent always visible.
func (a *Agent) SetStripSpawn(fn func() bool) { a.stripSpawn = fn }
```

In `internal/agent/loop.go`, replace the `Tools: a.tools.Schemas(),` line inside the `req := model.CompletionReq{...}` construction (~:159) with a filtered assembly. Change:

```go
		req := model.CompletionReq{
			Model:     a.modelID,
			Messages:  a.withSystem(messages),
			Tools:     a.tools.Schemas(),
			MaxTokens: a.maxTokens,
		}
```

to:

```go
		schemas := a.tools.Schemas()
		if a.stripSpawn != nil && a.stripSpawn() {
			schemas = withoutSpawnAgent(schemas)
		}
		req := model.CompletionReq{
			Model:     a.modelID,
			Messages:  a.withSystem(messages),
			Tools:     schemas,
			MaxTokens: a.maxTokens,
		}
```

Add a small helper at the bottom of `loop.go`:

```go
// withoutSpawnAgent returns schemas with any "spawn_agent" entry removed,
// preserving order. It allocates a fresh slice so it never mutates the
// registry's backing array. Returns the input unchanged when spawn_agent is
// absent (the common stripped-child case).
func withoutSpawnAgent(schemas []model.ToolSchema) []model.ToolSchema {
	present := false
	for _, s := range schemas {
		if s.Name == "spawn_agent" {
			present = true
			break
		}
	}
	if !present {
		return schemas
	}
	out := make([]model.ToolSchema, 0, len(schemas)-1)
	for _, s := range schemas {
		if s.Name == "spawn_agent" {
			continue
		}
		out = append(out, s)
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run 'TestRun.*SpawnAgent' -v`
Expected: PASS

- [ ] **Step 5: Full agent-package test**

Run: `go build ./... && go test ./internal/agent/`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/agent/agent.go internal/agent/loop.go internal/agent/loop_test.go
git commit -m "feat(agent): strip spawn_agent from per-turn schemas when predicate trips"
```

---

## Task 5: Wire the strip predicate and configured concurrency in cmd/fuse; static depth stripping

**Files:**
- Modify: `cmd/fuse/shell.go` (tree construction ~:133; child spawn tool registration ~:159-164; predicate wiring for root and children)
- Modify: `cmd/fuse/run.go` if the cleanest injection point is inside `buildAgentCore`/`buildChildAgent` (they call `agent.New`); otherwise wire at the shell call sites.
- Test: exercised end-to-end by Task 6; this task is verified by `go build ./...` and manual reasoning (cmd/fuse has thin/no unit tests — do not invent a brittle one).

**Interfaces:**
- Consumes: `config.AgentsConfig.MaxConcurrent` (Task 1); `agent.NewAgentTreeWithConcurrency` (Task 2); `agent.NewStripSpawnPredicate` (Task 3); `(*agent.Agent).SetStripSpawn` (Task 4); `agent.MaxDepth`; `childNode.Depth`.
- Produces: root and every child agent carry a strip predicate built from the same tree and configured `MaxConcurrent`; a child at `Depth == MaxDepth` has no `spawn_agent` in its registry.

- [ ] **Step 1: Confirm current behavior (no test to fail — reason from build)**

There is no existing cmd/fuse unit test harness for the spawn wiring. Verify the current build compiles before changes:

Run: `go build ./cmd/fuse/`
Expected: PASS (baseline).

- [ ] **Step 2: Implement — configured tree size**

In `cmd/fuse/shell.go` at ~:133, change:

```go
	tree := agent.NewAgentTree(alias, alias)
	tree.SetMaxSpawns(cfg.Agents.MaxSpawns)
```

to:

```go
	tree := agent.NewAgentTreeWithConcurrency(alias, alias, cfg.Agents.MaxConcurrent)
	tree.SetMaxSpawns(cfg.Agents.MaxSpawns)
```

- [ ] **Step 3: Implement — static depth strip for children**

In `cmd/fuse/shell.go`, at the child registry build (~:159-164), the child's `spawn_agent` is registered unconditionally:

```go
	childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), tree.SpawnBudget))
```

Guard it so a child created at MaxDepth never receives the tool. Note `childToolReg` may already contain a `spawn_agent` copied from the parent via `Clone()`/`Subset()` — so at MaxDepth we must Unregister rather than merely skip registration:

```go
	if childNode.Depth >= agent.MaxDepth {
		// Depth strip (static): a child at MaxDepth can never spawn — never give
		// it the tool, and drop any copy inherited from the parent's registry.
		childToolReg.Unregister("spawn_agent")
	} else {
		// Replace spawn_agent with one wired to the child's spawner.
		childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), tree.SpawnBudget))
	}
```

(Confirm `Registry.Unregister` exists — it does, at `internal/tools/registry.go:47`. Confirm `childToolRegistry` produces a registry that may carry an inherited `spawn_agent`: `Clone()` copies all tools including spawn_agent; `Subset()` force-includes spawn_agent. So Unregister is required, not optional.)

- [ ] **Step 4: Implement — install the strip predicate on root and children**

The root agent is built after `agent.New` returns via `buildAgentCore`. The simplest race-free injection is to set the predicate on the returned `*agent.Agent` at the shell call sites, since the tree and `cfg.Agents.MaxConcurrent` are both in scope there.

For the root agent: after the root `*agent.Agent` is constructed in the shell (locate where `buildAgentCore`/`buildAgentWithRendererAndTrace` is called for the root turn), add:

```go
	a.SetStripSpawn(agent.NewStripSpawnPredicate(tree, cfg.Agents.MaxConcurrent))
```

For child agents: inside the `WithChildBuilder` closure in `shell.go`, after `a` is built (~:187, right after the `if aerr != nil` check) and before `a.Run(...)`, add:

```go
	a.SetStripSpawn(agent.NewStripSpawnPredicate(tree, cfg.Agents.MaxConcurrent))
```

This covers acceptance criterion 1 ("root included") because the root agent gets the predicate too, and criterion 2 (permanent budget strip) because the same predicate checks `SpawnBudget()`.

Note: if `buildAgentCore`/`buildChildAgent` already have `tree` and `cfg` in scope at their definitions, prefer setting the predicate there so no call site is missed; otherwise the call-site wiring above is authoritative. Whichever location is chosen, ensure BOTH the root turn agent and every child agent receive it. Grep for every `agent.New(` / `buildAgentCore(` / `buildChildAgent(` / `buildAgentWithRendererAndTrace(` reachable from the interactive shell to confirm complete coverage.

- [ ] **Step 5: Build**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 6: Full suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add cmd/fuse/shell.go cmd/fuse/run.go
git commit -m "feat(fuse): wire strip predicate on root+children, static depth strip, configured concurrency"
```

---

## Task 6: Regression test — cap boundary, budget exhaustion, and backstop still fires while stripped

**Files:**
- Modify: `internal/agent/strip_test.go` (add regression tests)
- Test: same file.

**Interfaces:**
- Consumes: `NewStripSpawnPredicate` (Task 3); `Spawner.Spawn` + `ErrSpawnBudgetExhausted`/`ErrMaxDepthExceeded` (`spawn.go`); tree helpers.
- Produces: no new production symbols — this task only asserts the composed behavior of Tasks 2-4 against the two brakes and the backstop.

- [ ] **Step 1: Write the regression tests**

Add to `internal/agent/strip_test.go`:

```go
// Reversible cap: strip engages at the cap, then releases when a child finishes.
func TestStripReversibleAtActiveCap(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	p := NewStripSpawnPredicate(tr, 2)

	c1 := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning}
	c2 := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning}
	tr.addNode(c1)
	tr.addNode(c2)
	if !p() {
		t.Fatal("at cap: expected strip")
	}
	// A child finishes -> active count drops below cap -> tool returns.
	c1.Finish(StatusDone, "")
	if p() {
		t.Fatal("below cap after finish: expected NO strip (reversible)")
	}
}

// Permanent budget: once used >= max, strip stays engaged even with no active
// children.
func TestStripPermanentAtBudgetExhaustion(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(2)
	p := NewStripSpawnPredicate(tr, 16)

	// Two finished (not active) children exhaust the append-only budget.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	if r, pend := tr.ActiveCounts(); r != 0 || pend != 0 {
		t.Fatalf("expected zero active, got running=%d pending=%d", r, pend)
	}
	if !p() {
		t.Fatal("budget exhausted with zero active: expected permanent strip")
	}
}

// Backstop: a spawn call that sneaks through while stripped (e.g. a
// hallucinated call, or an in-flight turn that saw the schema) still gets the
// budget-exhausted error. Confirms stripping did not replace the enforcement.
func TestBackstopFiresWhenBudgetExhaustedWhileStripped(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(1)
	// Exhaust the budget: one child already created.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})

	p := NewStripSpawnPredicate(tr, 16)
	if !p() {
		t.Fatal("precondition: budget should be exhausted -> stripped")
	}

	root := tr.Node(tr.RootID())
	s := NewSpawner(WithTree(tr), WithNode(root), WithSpawnDepth(0))
	_, err := s.Spawn(context.Background(), SpawnOpts{Label: "sneaky", Task: "do"})
	if !errors.Is(err, ErrSpawnBudgetExhausted) {
		t.Fatalf("expected ErrSpawnBudgetExhausted backstop, got %v", err)
	}
}

// Backstop: a spawn at MaxDepth still errors even though depth stripping is the
// primary mechanism (static registry omission happens in cmd/fuse, not here).
func TestBackstopFiresAtMaxDepth(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	root := tr.Node(tr.RootID())
	// A spawner at depth MaxDepth would create a child at MaxDepth+1.
	s := NewSpawner(WithTree(tr), WithNode(root), WithSpawnDepth(MaxDepth))
	_, err := s.Spawn(context.Background(), SpawnOpts{Label: "deep", Task: "do"})
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("expected ErrMaxDepthExceeded backstop, got %v", err)
	}
}
```

Add `"context"` and `"errors"` to the `strip_test.go` imports (alongside `"testing"`).

- [ ] **Step 2: Run tests to verify they fail (before Tasks 2-4 would already be in place)**

If Tasks 2-4 are committed, `TestStrip*` and the backstop tests should compile. Run:

Run: `go test ./internal/agent/ -run 'TestStrip|TestBackstop' -v`
Expected: PASS if Tasks 2-5 are complete. (These are regression assertions over already-built behavior; they exist to lock it against future edits, so they may pass on first run — that is acceptable for a regression task. If any FAIL, the failure points at a real gap in Tasks 2-5 to fix before proceeding.)

- [ ] **Step 3: (No new implementation)**

This task adds only tests. If a test fails, fix the corresponding production code from Tasks 2-5, then re-run.

- [ ] **Step 4: Full suite**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/strip_test.go
git commit -m "test(agent): regression — cap reversible, budget permanent, backstops fire while stripped"
```

---

## Self-Review

**Spec coverage:**
- Brake 1 (active cap, reversible), default 16, configurable via `agents.max_concurrent` — Tasks 1, 2, 3, 4, 5, 6. Yield mechanics untouched (Task 2 keeps `YieldSlot`/`UnyieldSlot`; Task 2 Step 5 runs the existing yield tests).
- Brake 2 (lifetime budget, permanent), default raised 16→64 — Task 1 (default + config), Task 3 (predicate), Task 6 (permanent-strip regression).
- Depth stripping (static, at construction) — Task 5 Step 3 (Unregister at `Depth >= MaxDepth`).
- Backstops retained — not removed anywhere; asserted in Task 6.
- Budget-line injection retained — `NewSpawnAgentToolWithBudget` wiring is untouched (Task 5 only guards WHETHER it is registered for MaxDepth children).
- Silent strip (no notice) — the loop filter injects no message (Task 4).
- Acceptance 1 (root included, reversible) — Task 5 Step 4 sets predicate on root; Task 6 reversible test.
- Acceptance 2 (permanent regardless of active count) — Task 6 permanent test.
- Acceptance 3 (child at MaxDepth) — Task 5 Step 3.
- Acceptance 4 (hallucinated call still errors) — Task 6 backstop tests.
- Acceptance 5 (defaults + overridable + explicit kept) — Task 1 tests.
- Acceptance 6 (deepseek-flash scenario: zero refused, zero dead permission entries) — emergent from Tasks 4+5: once stripped, the model never sees the tool, so it neither generates approval entries nor triggers refusals; the composed predicate (budget term) is exercised by Task 6. No dedicated integration harness exists in cmd/fuse, so this is verified by the unit-level composition rather than a live replay.

**Placeholder scan:** No TBD/TODO; all code steps carry concrete code. The one soft spot — the exact config-loader entry point name in Task 1 (`loadFile` vs `Load`) — is flagged with an instruction to match the existing test helper, because the loader's public API name must be read from the file at implementation time rather than guessed.

**Type consistency:** `NewStripSpawnPredicate(tree *AgentTree, maxConcurrent int) func() bool`, `NewAgentTreeWithConcurrency(rootLabel, rootModel string, maxConcurrent int) *AgentTree`, `(*Agent).SetStripSpawn(func() bool)`, `AgentsConfig{MaxSpawns, MaxConcurrent int}` are used consistently across Tasks 1-6. `ActiveCounts() (running, pending int)` and `SpawnBudget() (used, max int)` match `tree.go`. `Registry.Unregister(name string)` matches `registry.go:47`. `withoutSpawnAgent([]model.ToolSchema) []model.ToolSchema` is local to `loop.go`.
