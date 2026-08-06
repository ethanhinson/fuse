---
id: 7
slug: scheduler-single-admission-authority
title: One Scheduler is the single admission, queueing, and throughput authority for subagents
status: Accepted
date: 2026-08-06
supersedes: []
reverses: []
relates_to: []
change: 35
---

## Context

Fuse spawns subagents from several independent triggers — workflow steps, fan-out
research passes, user-driven dispatch — and each of those call sites has its own
reasons to want a limit: a global brake on total concurrency, per-workflow pools,
rate gates against a provider, token/cost quotas. If each mechanism is implemented
where it is felt, the limits become free-standing: they live in different modules,
observe different counters, and cannot see one another. Two independent brakes
cannot compose — a global cap and a per-workflow pool enforced separately admit a
spawn that neither alone would forbid, or deadlock waiting on each other, and no
single place can answer "why was this subagent admitted (or held)?" Admission,
queueing, and throughput are the same decision viewed three ways, and splitting
them across the code that happens to trigger a spawn makes that decision
unanswerable and untestable.

## Decision

There is exactly **one Scheduler component**, and it is the single admission,
queueing, and throughput authority for every subagent spawn. Nothing spawns a
subagent except by asking the Scheduler to admit it; the Scheduler owns the queue
and the throughput accounting.

Every spawn limit — the global concurrency brake, per-workflow pools, rate gates,
and token/cost quotas — is a **policy the Scheduler enforces**, never a
free-standing mechanism living at a call site. A new limit is expressed as a policy
the Scheduler evaluates at admission time, not as a counter or gate bolted onto the
code that triggers the spawn. The rule for a reader: if you are adding or reasoning
about a subagent limit, you are adding a Scheduler policy — there is no other place
a spawn limit may live.

## Consequences

- **Composability.** All limits are evaluated together at one admission point, so a
  global brake, a workflow pool, a rate gate, and a token quota compose correctly —
  a spawn is admitted only if every applicable policy allows it. No two independent
  brakes can disagree or deadlock, because there is only one decision.
- **One answer to "why."** Admission, queueing, and throughput questions have a
  single authority to interrogate, testable in isolation from the triggers that feed
  it. Policies can be unit-tested against the Scheduler without standing up the
  workflows that would spawn.
- **Discipline at the call sites.** Every spawn path must route through the
  Scheduler; a call site may not enforce its own limit "just here." This costs some
  directness — a trigger cannot short-circuit a spawn locally — and requires every
  future limit to be modeled as a policy rather than an inline check.
- **A single point of contention.** Concentrating admission in one component makes
  it a hot path and a coordination bottleneck by design; its throughput and
  fairness behavior must be deliberately engineered rather than emergent.
