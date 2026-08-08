<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0023 — Shared result blackboard for inter-agent communication](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0023-agent-blackboard.md)**
<!-- docket:backlink:end -->

# Implementation plan — 0023 agent-blackboard

**Change:** [#0023 — Shared result blackboard for inter-agent communication](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0023-agent-blackboard.md)
**Spec:** `docs/superpowers/specs/2026-08-08-agent-blackboard-design.md` (on `docket`)
**Branch:** `feat/agent-blackboard`

> **Skill-layer note.** The configured plan/build/review/finish skills
> (`superpowers:*`) are not installed on this machine. Per the docket
> Skill-layer missing-skill rule this run degrades each to `auto`: the plan is
> authored inline and executed with TDD directly. Recorded here and in the PR
> body.

## Approach

Build bottom-up so each layer is testable in isolation before the layer above
wires it:

1. The `Blackboard` store (pure Go, `-race` tested with `tree == nil`).
2. The slot-yield `Wait` behavior against a real `AgentTree` (the highest-risk
   deadlock detail — its own task with a saturation regression).
3. The `blackboard` built-in tool (five discrete tool names, tool-error
   discipline).
4. Wiring into every `cmd/fuse` agent entry point (root + child builders),
   enumerated by grep at build time.
5. The TUI Blackboard tab.
6. End-to-end model-sees-tool verification at the gateway seam.

TDD throughout: each task writes the failing focused test(s) first, then the
implementation, then runs the package tests (with `-race` for concurrency
tasks). A final full-suite `go build ./... && go test ./...` gates the branch.

## Tasks

### Task 1 — `Blackboard` store: Put/Get/Delete/Keys/Snapshot

**Files:** `internal/agent/blackboard.go`, `internal/agent/blackboard_test.go`

- Test first (`-race`): concurrent `Put`/`Get` is race-safe; `Get` on a missing
  key returns `(zero, false)`; provenance (`WriterID`/`WriterLabel`/`WrittenAt`)
  is recorded; last-writer-wins on repeated `Put`; `Keys` glob via `path.Match`
  (exact, prefix `foo/*`, `*`/empty ⇒ all, no-match ⇒ empty), sorted;
  `Snapshot` returns an **independent copy** (mutating the returned map or its
  entries does not affect the store).
- Implement `BlackboardEntry` and `Blackboard` per the spec (single
  `sync.RWMutex`, `entries map[string]BlackboardEntry`, `waiters
  map[string][]chan struct{}`, `tree *AgentTree`). `Wait` is stubbed to a later
  task or written minimally here and fully exercised in Task 2.
- Constructor: `NewBlackboard(tree *AgentTree) *Blackboard` (tree may be nil).

**Done when:** `go test -race ./internal/agent/ -run Blackboard` green.

### Task 2 — `Blackboard.Wait`: value/timeout/cancel + no lost wakeup + slot yield

**Files:** `internal/agent/blackboard.go`, `internal/agent/blackboard_test.go`
(extend), a regression in the agent package.

- Test first (`-race`), with `tree == nil` for the pure-timing cases:
  - `Wait` returns immediately when the key is already set.
  - `Wait` returns the value on a **late** `Put` (producer writes after the
    consumer blocks).
  - `Wait` returns a timeout error when the key is never set (bounded, fast test
    timeout).
  - `Wait` returns `ctx.Err()` on context cancel.
  - **No lost wakeup:** a `Put` racing the has-key re-check still wakes the
    waiter (stress loop under `-race`).
- Implement `Wait(ctx, key, timeout)`: register the waiter channel **before**
  the has-key re-check (lost-wakeup guard); if set, return immediately; else
  `tree.YieldSlot(node?)` — see note — then `select` on
  `key-set | ctx.Done() | time.After(timeout)`, reacquiring via
  `tree.UnyieldSlot` in a `defer` on every path. `Put` signals/closes registered
  waiter channels under the lock.
  - **Slot-yield handle.** `Wait` needs the *calling agent's* `*AgentNode` to
    yield its slot (yield is per-node). Thread it in: `Wait(ctx, key, timeout,
    node *AgentNode)` **or** carry the node on the tool wrapper and have the tool
    pass it. Decision: keep `Blackboard.Wait(ctx, key, timeout, node)` with
    `node`/`tree` nil-safe (skip yield when either is nil, still block/time-out
    correctly). This keeps the store honest and the tool thin.
- **Slot-yield regression (highest-risk, own test):** with a real
  `AgentTree` at a small concurrency cap, saturate the cap with producer nodes,
  have a consumer `Wait` on a key a producer will set, and assert the whole
  thing **completes** rather than deadlocks — the freeze shape from 0012's
  `scheduler_queue_test.go`. Assert the consumer's slot was yielded while
  blocked (via the scheduler's observable slot/yield counters).

**Learnings applied:** `slot-cap-yield-while-blocked-on-children`.

**Done when:** `go test -race ./internal/agent/` green, including the saturation
regression.

### Task 3 — `blackboard` built-in tool (five discrete names)

**Files:** `internal/tools/blackboard.go`, `internal/tools/blackboard_test.go`

- Modeled on `internal/tools/spawn_agent.go`. The tool holds a `*Blackboard`
  handle plus the calling node's `ID`/`Label` (provenance), passed at
  construction: `NewBlackboardTools(bb *agent.Blackboard, nodeID, nodeLabel
  string, node *agent.AgentNode) []tools.Tool` returning the five tools — OR one
  constructor per name. Decide at build: a small internal shared struct with a
  per-name `op`, exposed as five `Tool`s each with its own `Name()`.
  - **Import-cycle check.** `internal/tools` importing `internal/agent` — verify
    no cycle (spawn_agent avoids it via the injected `SpawnFunc`). If a cycle
    exists, inject a narrow interface (`blackboardStore`) the same way
    `SpawnFunc` breaks the spawn cycle, and pass the `*AgentNode`/yield through
    that seam. This is a **gate at the start of the task** — resolve before
    writing the tool.
- Test first: for each op, tool-error (never panic) discipline —
  - `blackboard_write(key, value)`: `value` is a JSON string parsed with
    `json.Unmarshal`; malformed JSON ⇒ `Result{IsError:true}`; empty key ⇒
    error; success ⇒ `Put` with provenance, confirmation output.
  - `blackboard_read(key)`: returns JSON-encoded value or a "key not set"
    (non-error) result.
  - `blackboard_wait(key, timeout)`: timeout **required**, positive, bounded by
    a sane ceiling; non-positive/over-ceiling/missing ⇒ tool error; returns
    value or a timeout/cancel result.
  - `blackboard_keys(pattern)`: returns the glob-matched, sorted key list.
  - `blackboard_delete(key)`: idempotent removal, confirmation output.
- Each tool `Name()`/`Description()`/`Parameters()`/`Execute()` implemented;
  `var _ tools.Tool = ...` assertions.

**Done when:** `go test ./internal/tools/ -run Blackboard` green.

### Task 4 — Ownership + wiring in every agent entry point

**Files:** `cmd/fuse/main.go`, `cmd/fuse/shell.go`, `cmd/fuse/research_probe.go`
(+ shared helpers in `cmd/fuse/run.go` if needed), a wiring assertion test in
`cmd/fuse`.

- **Re-grep the sites first** (`grep -rn WithChildBuilder cmd/fuse/*.go` and the
  root `NewSpawnAgentTool*` registrations) — do not trust this list; the
  reconcile recorded main.go/shell.go/research_probe.go but the grep is
  authoritative (learning `patch-every-cloned-child-builder`).
- Create **one** `Blackboard` per session, owned by the root `AgentTree`
  (`agent.NewBlackboard(tree)`), immediately after the tree is constructed, in
  each of the three entry points.
- Register a **root-node-wired** blackboard tool set (provenance = rootNode) in
  each root `toolReg`.
- In each `WithChildBuilder` closure, **re-register** the blackboard tool set
  wired to `childNode` (provenance), exactly like `spawn_agent` is re-wired —
  because the tool carries per-node provenance it must not merely ride on
  `childToolRegistry`'s Clone/Subset of the parent registry. Do this **after**
  the subset/clone so it binds to the child.
- **Always wired, not spawn-gated:** the blackboard tool is NOT behind
  `shouldWireChildSpawn` / the depth strip — a child that cannot spawn still gets
  the blackboard. The one exception: an explicit `tools` subset that omits the
  blackboard tools is honored (the subset mechanism already drops unlisted tools;
  we simply do not force-include when the subset excludes them). Implement this
  by force-including on Clone (no subset) and root, and respecting the subset
  otherwise.
- **Wiring assertion test:** enumerate the builder sites (or exercise each entry
  point's registry construction) and assert every blackboard tool name is
  present at root and in a child built with the default (no-subset) path; assert
  it is present even when child spawning is withheld
  (`shouldWireChildSpawn`-false path); assert it is absent when an explicit
  `tools` subset excludes it.

**Learnings applied:** `patch-every-cloned-child-builder`,
`nil-safe-optional-tool-registration`.

**Done when:** `go build ./... && go test ./cmd/fuse/` green.

### Task 5 — TUI Blackboard tab

**Files:** `internal/tui/agents_model.go` (+ any tab helper),
`internal/tui/agents_model_drilldown_test.go` or a new
`internal/tui/blackboard_tab_test.go`.

- Thread a `*agent.Blackboard` into the agents model (a `WithBlackboard` option
  mirroring `WithTree`).
- Add a **"Blackboard"** tab/section to the agent-tree detail pane following the
  existing tab/drilldown pattern. Render `Snapshot()`: each key, its value
  (JSON-rendered, truncated/hard-wrapped to pane width), and an
  **agent-wrote indicator** from `WriterLabel` (e.g.
  `key  ⟨written by research/facet-2⟩`).
- All value/label bytes are model/tool-controlled ⇒ run through the existing
  `sanitizeDisplay` and hard-wrap before render (learning
  `sanitize-untrusted-bytes-fixed-width-tui`).
- Test first: a `teatest` render (or a direct model `View()` test) asserting
  keys, values, and wrote-by indicators appear, and that an oversized/untrusted
  value is wrapped and sanitized (no raw ESC/control bytes, no line over pane
  width). Force `termenv.TrueColor` around any `View()` capture (learning
  `teatest-final-frame-via-finalmodel-view`).

**Done when:** `go test ./internal/tui/ -run Blackboard` green.

### Task 6 — Directed-message inbox convention (thin helper + doc)

**Files:** doc note in the spec-adjacent area or a small helper in
`internal/agent/blackboard.go`; a test.

- The inbox is a **convention over the store**, not a new structure: a sender
  writes `inbox/<target>/<seq>` and the target reads
  `blackboard_keys("inbox/<self>/*")` + `blackboard_read`. Provide only a thin
  helper if it removes real duplication (e.g. an `InboxKey(target, seq)` /
  `InboxPattern(self)` pair) plus a short usage note; no blocking, no delivery
  guarantees, no agent-loop change.
- Test the helper's key/pattern shaping round-trips through `Keys`/`Get`.
- Keep minimal — the store + glob already do the work.

**Done when:** helper test green; convention documented.

### Task 7 — End-to-end model-sees-tool verification

**Files:** a `cmd/fuse` e2e test using the scripted-gateway pattern, or a
documented manual verification if the harness is heavy.

- Per learning `verify-tool-loop-at-gateway-seam`: drive the **real binary**
  against a scripted `LLM_GATEWAY_URL` double that logs each request's `tools[]`,
  and assert the `blackboard_write/read/wait/keys/delete` tools appear for both a
  root turn and a child turn. This exercises the `cmd/fuse` wiring the teatest
  harness cannot reach.
- If an existing gateway-double test rig exists in the repo, extend it; otherwise
  add a focused one.

**Done when:** the e2e assertion passes (or the manual verification is recorded
in the results file with the captured `tools[]` log).

### Task 8 — Full-suite gate + review prep

- `cd` to the feature worktree; run `go build ./...` and
  `go test ./...` (with `-race` at least on `internal/agent` and
  `internal/tools`). All green is the branch gate.
- Self-review the whole diff before opening the PR.

## Out of scope (do not build)

Cross-session persistence; ACLs; value size caps; smarter wait liveness
(tree-idle / producer-death wake); real mid-run message injection; ACP interop.
(Per the change's confirmed Out-of-scope list.)

## Risks

- **Deadlock via blocking wait** — dominant risk; mitigated by Task 2's mandatory
  slot-yield + saturation regression.
- **Import cycle** `internal/tools` → `internal/agent` — resolved at Task 3's
  gate (narrow injected interface if needed, mirroring `SpawnFunc`).
- **Missed wiring site** — mitigated by the grep-at-build-time rule and Task 4's
  wiring assertion.
- **Lost wakeup** — mitigated by register-before-recheck + signal-under-lock in
  Task 2.
