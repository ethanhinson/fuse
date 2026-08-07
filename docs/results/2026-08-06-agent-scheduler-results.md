<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0036 — Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0036-agent-scheduler.md)**
<!-- docket:backlink:end -->

# Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits — results
Change: #36 · Branch: feat/agent-scheduler · PR: <url> · Plan: docs/superpowers/plans/0036-agent-scheduler.md · ADRs: 7, 8, 9

## Verify (human)

Automated suites are green (`go build`, `go vet` fully clean, `go test -race ./...` across
all packages), but the schema-strip and queue behavior deserve the gateway-seam rig the
learnings mandate (the teatest harness fakes the Completer seam and cannot reach this
wiring). Suggested checks with a scripted `LLM_GATEWAY_URL` double that logs each
request's `tools[]`, built from THIS worktree:

- [ ] **Queue + fairness live**: with `.fuse.local.yml` `agents: {max_concurrent: 1,
  queue_bound: 2}`, script a parent that spawns 3 children in one turn — the third
  should be refused (`spawn queue at bound...` / `ErrQueueBoundExceeded` text) while the
  first two run/queue; `spawn_agent` should vanish from the parent's `tools[]` on the
  next turn while the queue sits at bound and reappear once it drains (ADR-0009: this
  queue-to-bound visibility applies to the global pool; a workflow pool still strips at
  its `concurrent`).
- [ ] **Rate gate live**: set `throughput: {requests_per_minute: 2}`; a scripted 4-turn
  loop should show turns 3+ delayed by the bucket (and Ctrl-C during a gated wait must
  cancel cleanly). Unset the block and confirm zero added latency.
- [ ] **Token quota live**: set `throughput: {session_tokens: <small>}`; after the
  ceiling hits, every subsequent spawn_agent result carries the `token quota exhausted:
  ...` line, `spawn_agent` disappears from `tools[]` globally, and no mid-turn abort
  occurs. Status bar / agents overlay (o) should show the new scheduler segments
  (`N/M slots · K queued · Xk/Yk tokens`, per-provider rate utilization).

## Findings

- **Review verdict MERGEABLE; five should-fix items, all addressed in `6846368`** —
  queue-bound TOCTOU closed (bound now enforced atomically at enqueue under the
  scheduler mutex, with a concurrent-overshoot regression test), tpm burst narrowed
  (Wait now charges a `len(body)/4` estimate, Report reconciles to
  `max(estimate, actuals)` — ADR-0008), trusted negative throughput values clamped
  (matching the MaxSpawns idiom), the two `workflowActivation` value-receiver vet
  findings fixed (vet is now fully clean, including pre-existing findings), provider
  rate keys now require a word boundary (`deepseek` matches `deepseek-flash`, not
  `deepseek2-chat`), the global slots figure in snapshots now reads the scheduler's own
  slot count (no more `18/16` under yielded parents), the status segment actually picks
  the busiest pool, and the agents-overlay header is height-clamped.
- **Deliberate semantic shift (ADR-0009)**: the global active-cap no longer strips the
  spawn_agent schema — it queues (visible) and the strip moved to the queue bound.
  Workflow pools deliberately retain 0034's strip-at-`concurrent`. One 0033-visible
  behavior change, zero 0034-visible changes.
- **Depth now strips proactively**: a node at its depth limit loses the schema via the
  unified predicate where 0033/0034 only denied at call time — sanctioned by the spec's
  "all strip variants collapse" clause; noting it as model-visible.
- **Warning-line scope**: the quota warning rides the existing budget-line seam
  (spawn_agent results). An exhausted agent with no completing spawn sees only the
  silent strip — the spec's "appended to subsequent tool results in scope" read
  narrowly, matching the budget-line pattern the spec itself cites.
- **Known deviation (review SF-4)**: Acceptance 1 ("no other code touches the semaphore
  or budget counters") holds — `spawnSem` is gone and `maxSpawns` lives only in the
  scheduler — but 0034's atomic workflow pool-total reservation backstop
  (`workflowActivation.reserved`, `cmd/fuse/workflow.go`) remains an admission decision
  outside the Scheduler, and `Spawner.Spawn` still carries inline depth/budget backstop
  checks rather than switching on one `Admit` verdict. Behavior-preserving residue,
  recorded as a follow-up rather than refactored at the end of an already-large branch.

## Follow-ups

- Fold the 0034 pool-total reservation and the Spawn-time backstop checks into a single
  scheduler-side `Admit`/reserve path (completes Acceptance 1 to the letter).
- Rate-gate the auto-mode classifier adapter if its volume ever grows (deliberately
  ungated; documented in `cmd/fuse` `sessionRateGate`).
- Spec open questions left open deliberately: rate gate stays default-off pending
  real-world 429 data; `queue_bound` stays a multiplier; session-ceiling halt stays
  strip + warning (no synthesis prompt).
- One-shot/headless paths surface no scheduler counters (no TUI); consider a `--stats`
  line if observability is wanted there.

## Build-loop notes

- Plan/build/review roles ran under the missing-skill degradation (superpowers skills
  unavailable in the driving session): plan authored directly, build executed
  task-by-task by docket-build profile agents (premium for the concurrency tasks),
  review by a dedicated whole-branch reviewer. All eight plan tasks completed with
  RED/GREEN evidence and mutation-tested guards (convoy, depth-2 deadlock, cancellation
  leak, sibling isolation, warning scope, queue-bound overshoot, tpm reconciliation).
