---
id: 8
slug: rate-gate-per-logical-request-tpm-steady-state
title: Rate gate charges per logical request; tpm is a steady-state guarantee, not an instantaneous one
status: Accepted
date: 2026-08-06
supersedes: []
reverses: []
relates_to: [7]
change: 36
---

## Context

Change 0036 adds a token-bucket rate gate (`internal/ratelimit.Bucket`, injected
into `internal/model.Adapter` through the model-defined `RateGate` interface) at
`Adapter.Complete` — the single dispatch choke point through which every agent-turn
model request flows. As the concrete throughput policy behind ADR-0007's single
admission authority, the gate needs a recorded rule for two coupled questions that
its placement and its charging model force:

1. **Where in the dispatch path is the gate consulted?** `Adapter.Complete` wraps a
   transport retry loop that re-issues on transient failures, including provider
   429s. Metering could sit outside that loop (once per logical request) or inside
   it (once per transport attempt).

2. **What does the gate charge for tokens, given that the true token cost of a
   request is not known until the gateway reports actuals?** The request payload is
   knowable up front (a cheap estimate); the output-token count is not. A gate that
   only ever charges what it can see up front would systematically under-count; one
   that waited for actuals could not gate before dispatch at all.

## Decision

**The gate meters logical model requests, not transport attempts, and it charges
per logical request.**

1. **Consult once, before the retry loop.** The gate is consulted exactly once per
   `Adapter.Complete` call, *before* the transport retry loop runs. Transport
   retries — including 429 retries — are **not** re-gated. Re-charging a retry would
   double-charge the caller for the provider's own failure and starve recovery
   exactly when the provider is already struggling.

2. **tpm is estimate-then-reconcile.** `Wait` charges a conservative up-front
   estimate for tokens (request payload `len(body)/4`; callers may pass `0`).
   `Report` then reconciles against the gateway-reported actuals, charging **only
   the positive delta**. Total charged is therefore `max(estimate, actuals)` and is
   **never refunded below the reservation** — so the scheme is not gameable by
   over-reserving and under-reporting.

3. **rpm is strictly enforced** (whole requests), because a request is a countable,
   fully-known unit at admission time.

4. **The auto-mode classifier adapter is deliberately un-gated.** It is a low-volume,
   dedicated verdict path — not the agent-turn choke point — so it is left out of the
   rate gate on purpose. This is a recorded residue, not an oversight.

The rule for a reader: **`tpm` is a steady-state average guarantee, not a hard
per-instant ceiling.** Because the up-front estimate omits the output-token
remainder, N concurrent first dispatches can burst past the instantaneous tpm cap by
that un-estimated remainder. `Report` charges the delta as debt, driving the token
axis negative; refill must repay that debt before any further admissions. Over time
the configured tpm holds as an average — but at any single instant the cap may be
exceeded by the in-flight, not-yet-reconciled output tokens.

## Consequences

- **Retries are honest.** A caller pays for one logical request regardless of how
  many transport attempts (including 429 retries) it took, so provider flakiness does
  not inflate the caller's rate charge or block its recovery.
- **No gaming the reservation.** `max(estimate, actuals)` with no refund below the
  reservation means neither a large over-reserve nor a small under-report can win
  back budget; the accounting is monotonic per request.
- **tpm bursts are possible and accepted.** The instantaneous tpm ceiling can be
  exceeded by the un-estimated output remainder across concurrent first dispatches.
  Anyone who needs a hard per-instant token ceiling cannot rely on this gate for it;
  the guarantee on offer is a steady-state average enforced by debt-then-refill.
  `rpm` remains a hard ceiling.
- **The classifier path is unmetered.** Auto-mode classification traffic does not
  count against the configured limits. This is safe only while it stays low-volume
  and off the agent-turn choke point; moving it onto that path would require
  revisiting this decision.
- **One throughput policy, one place.** Consistent with ADR-0007, all of this lives
  as a Scheduler-side throughput policy at the single dispatch choke point rather
  than as ad-hoc checks scattered across spawn triggers.
