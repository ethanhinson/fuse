<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0036 — Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-07-0036-agent-scheduler.md)**
<!-- docket:backlink:end -->

# 0036 — Agent scheduler: global queue, fairness, and turn-level throughput

## Problem

Fuse's spawn-control primitives are accreting per feature rather than composing: a
channel semaphore with arrival-order wakeup and an unbounded implicit queue
(`internal/agent/tree.go`), a lifetime budget counter, 0033's schema-strip predicates
(global), and 0034's pool-scoped variants. Three gaps remain even after 0033+0034:

1. **Queue discipline.** Goroutines blocked on the semaphore drain in arrival order with
   no fairness across subtrees: a 15-child batch enqueued first convoys everything behind
   it, and the queue is unbounded.
2. **Reservation semantics.** 0034 pool slots are a cap, not a guarantee; nothing
   prevents freeform spawns from occupying capacity a workflow is about to need.
   Fairness, not carve-outs, is the v1 answer — but no fairness mechanism exists.
3. **Concurrency is not throughput.** Sixteen slots say nothing about requests or tokens
   per minute hitting the gateway. Sixteen fast agents in tight turn loops can exceed
   provider rate limits and burn spend at unbounded velocity while violating no cap fuse
   defines. Peers treat this as its own axis (Codex `token_budget`/`rollout_budget`;
   Claude Code's Workflow shared token pool).

Decision (recorded as an ADR alongside this change): **one Scheduler component is the
single admission, queueing, and throughput authority; every limit — global, workflow
pool, rate, quota — is a policy it enforces.** 0033 ships unchanged (its predicate
survives as a call into the scheduler); 0034 seeds the component (pool accounting is
implemented as the scheduler's first version); this change completes it.

## Design

### The Scheduler component

One type (seeded by 0034's pool accounting) owning all admission state: global slots
(0033: 16), per-pool slots and totals (0034), the global lifetime budget (0033: 64),
the run queue, and the throughput gates. Three call surfaces:

- **Admission** — `spawn_agent` execution asks to admit a spawn: `granted` (slot now),
  `queued` (bounded wait), or `denied` (budget/depth/quota — existing error messages
  preserved as the race backstop).
- **Visibility** — the per-turn schema-strip decision (0033/0034) becomes one predicate:
  `spawn_agent` is present in an agent's schemas iff an admission request from that
  agent's scope would currently be granted or queued within bound. All strip variants
  (global active, global budget, pool slots, pool total, depth, token quota) collapse
  into this single rule.
- **Throughput** — `Adapter.Complete` (`internal/model/adapter.go:212`, the single choke
  point every agent turn passes through) consults the rate gate before dispatch and
  reports usage after (per-node token counts already exist — `TokensIn`/`TokensOut`).

### Queue: bounded, FIFO within pool, round-robin across pools

- Every spawn request lands in its pool's FIFO queue (workflow subtrees are pools;
  freeform spawns share one implicit session pool).
- **Bound**: per-pool pending ≤ 2× the pool's slots (default; configurable). At the
  bound, the visibility predicate strips the schema — the model stops committing spawns
  it can't have — and a call that races through anyway is denied with the existing
  error.
- **Dequeue**: when a slot frees, pools with non-empty queues are served round-robin;
  FIFO within a pool. This kills the convoy problem without weights or priorities.
- **Reservations are caps, not guarantees (v1).** Round-robin makes starvation
  transient; guaranteed carve-outs (min-slots per pool) are deferred until a real need
  survives contact with fairness.

### Throughput: rate smoothing + hard quotas (both)

- **Rate gate** (smoothing): token-bucket limiter at `Adapter.Complete` — global
  `requests_per_minute` and `tokens_per_minute`, optional per-provider overrides
  (fuse fronts multiple providers through the gateway; limits differ). Agents block on
  the bucket (ctx-cancellable); queued turns consume nothing. Unset = unlimited
  (today's behavior).
- **Quotas** (hard ceilings): per-workflow `pool.tokens` and optional session
  `throughput.session_tokens`. Accounting is subtree/session sums of the existing
  per-node counters. Exhaustion semantics: the scope's `spawn_agent` strips permanently
  (workflow scope) or globally (session scope), and a machine-authored warning line —
  same pattern as the spawn budget line — is appended to subsequent tool results in
  scope so agents conclude with what they have. No mid-turn aborts: v1 stops *fan-out
  and fetch-more*, not thought in flight.

### Config sketch

```yaml
agents:
  max_concurrent: 16      # 0033
  max_spawns: 64          # 0033
  queue_bound: 2.0        # × slots, per pool (this change)
throughput:
  requests_per_minute: 0  # 0 = unlimited (default)
  tokens_per_minute: 0
  session_tokens: 0
  providers:              # optional per-provider overrides
    deepseek: { requests_per_minute: 60 }
workflows:
  research:
    pool: { concurrent: 5, total: 8, max_depth: 1, tokens: 500000 }  # tokens: this change
```

### Observability

The scheduler exposes counters (queued per pool, slots in use, rate-gate utilization,
quota spend) for the status bar and agents view — e.g. `research 3/5 slots · 2 queued ·
310k/500k tokens`. Display wiring is the implementer's call; the counters are not.

## Acceptance

1. All spawn admission, queueing, and stripping decisions flow through the Scheduler;
   no other code touches the semaphore or budget counters directly.
2. Convoy test: pool A enqueues 15, pool B then enqueues 3 — dequeue interleaves A/B
   round-robin rather than draining A first.
3. Queue bound: at 2× slots pending, the pool's agents lose the `spawn_agent` schema;
   a racing call gets the existing deny error; the schema returns as the queue drains.
4. Rate gate: N agents in tight loops never exceed configured rpm/tpm (fake-clock test);
   unset config means no gate and zero added latency.
5. Workflow token quota exhaustion strips `spawn_agent` in that subtree only and injects
   the warning line; `session_tokens` exhaustion strips globally. Existing runs with no
   `throughput:`/`tokens:` config behave byte-identically to 0034.
6. Round-trip with 0033/0034: their acceptance suites still pass with the scheduler as
   the enforcement backend.

## Out of scope

- Weighted or priority-class fairness; guaranteed min-slot reservations.
- Dollar-cost accounting; cross-session or persistent spend tracking.
- Mid-turn hard aborts on quota exhaustion.
- Provider-side adaptive throttling (reacting to 429s dynamically) — the gate is
  configured, not learned.

## Open questions

- Session-quota halt polish: is strip + warning line enough, or should the root get one
  explicit synthesis prompt when the ceiling hits?
- `queue_bound` as a multiplier vs an absolute count.
- Whether the rate gate should default on (conservative rpm) rather than unlimited once
  real-world gateway 429 data exists.
