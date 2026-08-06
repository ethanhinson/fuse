<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0036 — Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0036-agent-scheduler.md)**
<!-- docket:backlink:end -->

# Plan — 0036 Agent scheduler: global queue, cross-pool fairness, and turn-level throughput limits

> Spec: `docs/superpowers/specs/0036-agent-scheduler.md` (on `docket` branch)
> Change: 0036-agent-scheduler
> ADR: ADR-0007 (scheduler as the single admission authority) — already Accepted

## Approach & context

This change creates the **Scheduler** component ADR-0007 committed to: one type that is
the sole admission, queueing, and throughput authority, folding in 0033's global brakes
and 0034's pool machinery as policies it enforces. Build order follows the spec's three
call surfaces: the component + admission first (behavior-preserving refactor of what
exists), then the bounded fair queue (new behavior), then the unified visibility
predicate, then the throughput axis (rate gate + token quotas, config-gated off by
default), then observability.

Key existing seams (on `origin/main` @ c595d5e, present in this worktree):

- `internal/agent/tree.go` — `AgentTree` owns the channel semaphore (`spawnSem`,
  arrival-order wakeup), the global budget (`maxSpawns`/`SpawnBudget()`),
  `ActiveCounts()`, refcounted `YieldSlot`/`UnyieldSlot` (root exempt; change 0012),
  per-node `TokensIn`/`TokensOut` + `UpdateTokens`. `MaxConcurrentSpawns = 16`.
- `internal/agent/strip.go` — `NewStripSpawnPredicate` (global: permanent budget +
  reversible active-cap), `NewWorkflowStripPredicate` (subtree-scoped pool),
  `NewOrPredicate` composition, `WorkflowPool` (the config-mirror type; internal/agent
  never imports internal/config).
- `internal/agent/subtree.go` — `SubtreeActiveCounts`, `SubtreeSpawnCount`,
  `WorkflowRootOf` (innermost root wins).
- `internal/agent/spawn.go` — call-time backstops (`ErrMaxDepthExceeded`,
  `ErrSpawnBudgetExhausted`, 0034's atomic per-call total-quota reservation).
- `internal/model/adapter.go:212` — `Adapter.Complete`, the single dispatch choke point
  every agent turn passes through; `CompletionResp.InputTokens/OutputTokens` carry usage.
  NOTE the import direction: `internal/agent` imports `internal/model`, so the rate-gate
  hook must be an interface **defined in `internal/model`** and injected from `cmd/fuse`.
- `internal/config/schema.go` — `AgentsConfig{MaxSpawns,MaxConcurrent}`,
  `Workflows map[string]WorkflowConfig` with `PoolConfig{Concurrent,Total,MaxDepth}`,
  tighten-only local merge (ADR-0006).
- `cmd/fuse` — **three cloned child builders** wire child tool registries: `main.go`'s
  one-shot `run()`, `shell.go`, `research_probe.go`. Any wiring change lands in all
  three, with the site list re-derived by grep at fix time, never from this plan.

### Learnings that gate this change

- **slot-cap-yield-while-blocked-on-children** (#12): the queue must count *active*
  work, not *alive* holders. The scheduler inherits the yield/unyield refcounting
  unchanged in semantics; a resumed parent (`Unyield`) must never deadlock behind its
  own descendants — give reacquisition its own lane rather than the pool queues.
  Regression shape to keep green: fill the cap with parents that each spawn one child;
  assert completion.
- **patch-every-cloned-child-builder** (#34): enumerate `cmd/fuse` wiring sites by grep
  at fix time; there are at least three.
- **verify-tool-loop-at-gateway-seam** (#33): schema/strip changes are verified with the
  real binary against a scripted `LLM_GATEWAY_URL` double logging each request's
  `tools[]`; the teatest harness cannot reach this wiring. Plan for it in results.
- **bound-every-model-call** (#12): the rate gate blocks agents *before* dispatch — it
  must be ctx-cancellable so Ctrl-C still stops a gated call, and a gated wait should be
  visible (trace/status), not a silent stall.
- **mutex-test-double-concurrent-provider** (#10): any fake shared with goroutines locks
  both getter and setter.

### Design decisions to surface for ADRs (step 6)

- ADR-0007 already records the single-authority decision — cite it, don't re-record.
- **Rate-gate seam as a `model`-defined interface** (import-direction consequence) —
  candidate ADR if the shape ends up non-obvious.
- **Fair queue as slot-grant dispatch, not a second semaphore** — the scheduler replaces
  the bare channel-receive wakeup with an explicit grant queue; candidate ADR.

## Automated-test strategy

- `internal/agent`: scheduler unit tests — admission verdicts, convoy/round-robin
  interleaving (spec Acceptance 2), queue bound + visibility flip (Acceptance 3), yield
  regression, quota exhaustion strips (Acceptance 5). Fake clock injected for anything
  time-dependent; `go test -race` throughout.
- `internal/model`: rate-gate consultation in `Complete` (gate called before dispatch,
  usage reported after; nil gate ⇒ zero overhead) with a fake gate; token-bucket unit
  tests with a fake clock (Acceptance 4).
- `internal/config`: `queue_bound`, `throughput:` (global + per-provider), and
  `pool.tokens` parse/layering/tighten-only tests; absent config ⇒ zero values
  (Acceptance 5's byte-identical clause).
- Whole-suite round-trip: existing 0033/0034 tests pass unchanged except where they
  reach into seams the scheduler now owns (Acceptance 6) — semantic assertions stay.

---

## Task 1 — Scheduler component + admission (behavior-preserving)

**Goal:** `internal/agent/scheduler.go` defines `type Scheduler` owning global slots,
the lifetime budget, and per-pool policy; `spawn.go` asks it to admit every spawn;
nothing else touches the semaphore or budget counters directly (Acceptance 1). No
behavior change yet — arrival-order wakeup persists until Task 2.

- **Test first:** admission verdict tests — `granted` under the cap, `queued` at the
  cap, `denied` on exhausted budget/depth (existing error identities preserved); yield/
  unyield refcount semantics unchanged (port or keep the existing tree tests); the
  depth-2 freeze regression stays green.
- **Implement:** `Scheduler` constructed alongside the tree (constructor takes tree +
  global caps; per-workflow pools registered at activation, reusing `WorkflowPool`).
  Move slot acquire/release/yield/unyield behind scheduler methods; `AgentTree` keeps
  node data and counts. Grep for every direct `spawnSem`/`acquireSpawnSlot`/
  `YieldSlot`/`UnyieldSlot`/`SetMaxSpawns` caller (including `cmd/fuse`'s three
  builders) and route through the scheduler. Keep exported shims only where the tree
  API is exercised by tests that still express valid semantics.
- **Verify:** `go test -race ./internal/... ./cmd/...`; `go build ./...`.

## Task 2 — Bounded fair queue: per-pool FIFO, round-robin dispatch

**Goal:** spawn requests land in their pool's FIFO (workflow subtrees = pools; freeform
spawns share one implicit session pool); a freed slot is granted round-robin across
non-empty pool queues (Acceptance 2); per-pool pending is bounded at
`ceil(queue_bound × pool slots)` (default multiplier 2.0; the global/implicit pool uses
`max_concurrent` as its slot figure).

- **Test first:** convoy test — pool A enqueues 15, pool B then 3; dequeue interleaves
  A/B rather than draining A first. Queue-bound test — pending at the bound ⇒ admission
  reports over-bound (input for Task 3's strip; racing call still gets the existing deny
  error). Unyield-lane test — a resumed parent reacquires without queueing behind its
  own descendants' pending spawns (deadlock regression, fake-clock or channel-step
  driven, `-race`).
- **Implement:** replace the bare channel wakeup with an explicit grant dispatcher:
  waiters enqueue `{poolID, grantCh}`; release/yield events dispatch the next grant
  round-robin (rotating pool cursor), FIFO within a pool; unyield reacquisition gets a
  priority lane. Bound checked at enqueue. Pool identity via `WorkflowRootOf`; "" ⇒ the
  implicit session pool.
- **Verify:** `go test -race ./internal/agent/...`; whole suite.

## Task 3 — Unified visibility predicate

**Goal:** all strip variants collapse into one rule: `spawn_agent` is present in an
agent's schemas iff an admission request from that agent's scope would currently be
granted or queued within bound (Acceptance 3). 0033/0034 semantics preserved.

- **Test first:** port the existing strip-predicate tests to the unified predicate
  (global budget permanent, active-cap reversible, pool total permanent, pool
  concurrent reversible, depth static, sibling-subtree isolation) plus the new
  queue-bound term (strips at bound, returns as the queue drains — Acceptance 3's
  schema-returns clause).
- **Implement:** `Scheduler.Visible(nodeID) bool` (or a per-node predicate factory)
  computing the would-admit answer; `NewStripSpawnPredicate`/`NewWorkflowStripPredicate`
  become thin wrappers over it (or their call sites move to it — grep all three
  `cmd/fuse` builders plus loop wiring). Call-time deny in `Spawn` stays as the race
  backstop, error text unchanged.
- **Verify:** `go test -race ./internal/... ./cmd/...`.

## Task 4 — Config surface: `queue_bound` + `throughput:` + `pool.tokens`

**Goal:** the new knobs parse, layer, and default off: `agents.queue_bound` (float
multiplier, 0/unset ⇒ 2.0), `throughput.{requests_per_minute,tokens_per_minute,
session_tokens}` (0 = unlimited) with optional `throughput.providers.<name>` overrides,
`workflows.<name>.pool.tokens` (0 = unset). Absent config ⇒ byte-identical behavior
(Acceptance 5 tail).

- **Test first:** `internal/config` table tests — round-trip of the full sketch from
  the spec; absent keys ⇒ zeros; `.fuse.local.yml` tighten-only direction for every new
  numeric (lower rpm/tpm/tokens/queue_bound only, per ADR-0006), following the existing
  workflows-merge tests as the template.
- **Implement:** `ThroughputConfig` + per-provider map on `Config`; `QueueBound` on
  `AgentsConfig`; `Tokens` on `PoolConfig`; raw mirrors + normalization + tighten-only
  merge in the established pattern. Structured fields only (no free-text scalars).
- **Verify:** `go test ./internal/config/...`.

## Task 5 — Rate gate: token bucket at `Adapter.Complete`

**Goal:** N agents in tight turn loops never exceed configured rpm/tpm; unset config
means no gate and zero added latency (Acceptance 4).

- **Test first:** token-bucket unit tests with an injected fake clock (capacity, refill,
  rpm and tpm axes, per-provider override selection, ctx cancellation unblocks a
  waiter); `Adapter.Complete` tests with a fake gate — gate consulted before dispatch,
  usage reported after the response, nil gate ⇒ no calls, no allocation, no wait.
- **Implement:** in `internal/model`: `type RateGate interface { Wait(ctx context.Context,
  provider string, estTokens int) error; Report(provider string, inTokens, outTokens int) }`
  (exact shape the implementer's call — keep it minimal), optional field on `Adapter`,
  consulted at the top of `Complete` and reported after success. Token-bucket
  implementation lives with the scheduler side (`internal/agent` or a small leaf
  package) with a `clock func() time.Time` + waiter wakeup injectable for tests;
  `cmd/fuse` wires config → bucket → adapter in all binary paths (grep the builders).
  Queued (unstarted) turns consume nothing — the gate sits at dispatch, which gives
  this for free.
- **Verify:** `go test -race ./internal/model/... ./internal/agent/...`; whole suite.

## Task 6 — Hard token quotas: workflow `pool.tokens` + session ceiling

**Goal:** workflow-quota exhaustion strips `spawn_agent` in that subtree only and
injects a machine-authored warning line into subsequent tool results in scope; session
ceiling does the same globally. No mid-turn aborts (Acceptance 5).

- **Test first:** subtree token-sum accounting (existing per-node counters summed via
  the snapshot walk, mirroring `SubtreeSpawnCount`); visibility flips permanently at
  quota (workflow scope: that subtree only — sibling isolation asserted; session scope:
  global); warning line appended to tool results in scope after exhaustion, absent
  before, absent outside scope — locate the existing spawn-budget warning-line
  mechanism by grep and mirror its pattern and tests.
- **Implement:** `SubtreeTokens(rootID)` / session total on the scheduler; quota terms
  join the Task-3 visibility rule (permanent, like budget terms) and the call-time deny
  backstop; warning injection reuses the 0033 budget-line seam. In-flight turns finish
  untouched — enforcement points are visibility, admission, and the appended line only.
- **Verify:** `go test -race ./internal/...`; whole suite.

## Task 7 — Observability counters + display wiring

**Goal:** the scheduler exposes per-pool counters — queued, slots in use/total, rate-gate
utilization, quota spend (e.g. `research 3/5 slots · 2 queued · 310k/500k tokens`) — and
the status bar / agents view render them. Counter shape is contractual; display wiring
is the implementer's call.

- **Test first:** counter snapshot method returns correct values under a constructed
  tree/queue state (unit); a focused view test that the agents-view/status-bar model
  renders a non-empty scheduler summary (match the existing TUI test idiom).
- **Implement:** `Scheduler.Snapshot()` (per-pool + global struct, race-safe); wire into
  the status bar and agents tab where 0033/0034 already surface spawn state — grep for
  the existing budget/slots display to find the seam. Sanitize per the TUI learnings
  (fixed-width, no raw model bytes involved here).
- **Verify:** `go test ./internal/tui/... ./internal/...`; whole suite.

## Task 8 — Round-trip + full-suite gate

**Goal:** Acceptance 6 — the 0033/0034 acceptance suites pass with the scheduler as the
enforcement backend; the whole repo is green and race-clean.

- `go build ./... && go vet ./... && go test -race ./...`.
- Sweep for any remaining direct semaphore/budget access outside the scheduler
  (`grep -rn "spawnSem\|maxSpawns" --include=*.go` — hits only inside the scheduler's
  own file(s) and their tests).
- Record the gateway-seam manual verification steps (scripted `LLM_GATEWAY_URL` double
  logging `tools[]`; force a stripped call to prove the deny backstop) in the results
  file for the human merge gate.
