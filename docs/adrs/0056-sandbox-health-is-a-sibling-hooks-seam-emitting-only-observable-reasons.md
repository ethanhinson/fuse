---
id: 56
slug: sandbox-health-is-a-sibling-hooks-seam-emitting-only-observable-reasons
title: Sandbox health is a sibling HealthHooks seam, and only honestly-observable reasons are emitted
status: Accepted
date: 2026-09-04
supersedes: []
reverses: []
relates_to: [44, 55]
change: 65
---

## Context

Change 0074 (sandbox health emitter) was folded into change 0065 because a per-tenant mount was
expected to imply a persistent container. `event.SandboxHealthPayload` documents a closed six-reason
enum — `oom`, `runtime_exit`, `pull_failed`, `acquire_failed`, `unresponsive`, `recovered` — and the
metric `fuse_sandbox_unhealthy_total` was registered but had NO emitter. Change 0063 left a
deliberate tripwire test asserting the metric gains no series, with a message telling whoever adds an
emitter to flip it.

Two decisions were made, recorded together because they share one rationale: never fabricate a
signal.

## Decision

**1. Attach point — a sibling `HealthHooks`, not a fourth field on `PoolHooks`.**

`sandbox.PoolHooks` has three fields — `Acquired`, `Released`, `Reaped` — all *entry lifecycle*
events. Health fits none of them: `pull_failed` and `acquire_failed` fire with NO pool entry in
existence, and `oom`/`runtime_exit` fire mid-`Exec` while the entry stays checked out. Folding either
into `Released` would report a hand-back that did not happen and corrupt `fuse_sandbox_active`.
Health also arises at TWO layers — the Pool for acquire, the handler for pull and exec — which a
Pool-only struct cannot reach.

So: a sibling `HealthHooks` installed by `Service.SetHealthHooks`, mirroring the existing
`SetGateHooks`. `PoolSource` gained an unexported `healthHooks()` method, sealed like `gateFor`, so
only `*Service` can supply it. This resolves the open question change 0074 itself named.

**2. Which reasons are emitted — only those this substrate can observe.**

The substrate is still `docker run --rm` per `Exec`; no container outlives a command.

- Emitted: `pull_failed`, `acquire_failed`, and `oom`/`runtime_exit` via a new exit-code classifier
  (137 and other signal deaths). An ORDINARY non-zero command exit emits NOTHING, guarded by a live
  negative test.
- NOT emitted, and deliberately without even a declared constant: `unresponsive` and `recovered`,
  which presuppose probing a container between commands. `ContainerID` stays empty and
  `containerIdentified` stays unimplemented. All three defer to change 0074, per the spec's own
  if-and-only-if clause — which required amending the spec rather than faking the emitter; the spec
  carries that dated amendment.
- A third exclusion followed from review: `ErrNoTenantRoot` is excluded from `acquire_failed`, so a
  tenant-root MISCONFIGURATION does not drive an operator-facing health counter once per bash call.
  One incident, one reason, reported at the site that can classify it — the same rule the
  `errPullFailed` exclusion already followed. The startup notice is the correct channel for a config
  fault.

## Consequences

The metric is now genuinely fed, and the change-0063 tripwire is replaced by a positive assertion
driving a real OOM against a real container on a live `/metrics` scrape. Four of six reasons are
reachable; an operator reading the reason label sees only conditions this substrate can actually
observe, so the label is trustworthy rather than aspirational.

Change 0074 remains open for the persistence-dependent remainder. The cost: two enum values
documented in `event` have no producer — honest rather than tidy.

Produced by change 0065, commits 9e52e0f and 025c579 on `feat/bash-per-tenant-filesystem-isolation`.
