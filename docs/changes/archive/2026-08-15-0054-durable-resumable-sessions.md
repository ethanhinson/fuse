---
id: 54
slug: durable-resumable-sessions
title: Durable, resumable sessions — a conversation survives disconnect; refresh restores transcript + memory
status: done
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-15
depends_on: [53]
related: [47, 48, 49, 50]
discovered_from: [53]
adrs: []
spec: docs/superpowers/specs/2026-08-15-durable-resumable-sessions-design.md
plan: docs/superpowers/plans/2026-08-15-durable-resumable-sessions-plan.md
results: docs/results/2026-08-15-durable-resumable-sessions-results.md
trivial: false
auto_groomable:
branch: feat/durable-resumable-sessions
claimed_at: 
pr: 60
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-15-durable-resumable-sessions-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-durable-resumable-sessions-design.md) |
| Plan | [2026-08-15-durable-resumable-sessions-plan.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/2026-08-15-durable-resumable-sessions-plan.md) |
| Results | [2026-08-15-durable-resumable-sessions-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-08-15-durable-resumable-sessions-results.md) |
| PR | 60 |
<!-- docket:artifacts:end -->

## Why

Change 0053 made a conversation persist **across turns** on one `loop_id` — but only while
its owning goroutine and its **in-memory transcript** stay alive, and only while the
originating connection stays up. The parked loop's run context descends from the caller's
request/connection context (`inproc.go` derives `runCtx` from the `StartLoop(ctx)` argument),
so a client disconnect cancels that ctx: the park falls through to the terminal return, the
run goroutine flips the loop to finished in the durable registry (a later `Send` ⇒
`ErrLoopFinished`), and the in-memory `messages` transcript is discarded with the goroutine.
Cleanup is correct and leak-free — but the session is **pinned to the liveness of one
connection**.

That directly contradicts 0053's own north-star framing ("attach to your running loop from
your phone"; a conversation as a "first-class, resumable, server-side thing"). As shipped,
you cannot **refresh the page and get your conversation back**. Close the tab, lose the
session. 0053 explicitly scoped this out ("cross-instance session migration / durable park
state … deferred until 0049/0050 need it") — this change owns that deferred work.

The user-visible requirement: a user should be able to **refresh the screen (or reconnect
from another device) and see their transcript, with the loop's memory intact**, then keep
chatting on the same session.

## What changes

Design settled in [the spec](../../superpowers/specs/2026-08-15-durable-resumable-sessions-design.md);
proposal-altitude summary:

- **Sever session lifetime from connection lifetime.** An interactive loop runs under a
  **session-scoped context** distinct from the request ctx, so a disconnect no longer cancels
  the parked run. The session is bounded by an **idle TTL** (default 30 min; a single named
  constant, not yet a per-tenant knob) and reaped when idle.
- **Rebuild the transcript from the durable event stream — no second source of truth.** The
  model-facing `messages` are reconstructed from the #47 durable events (ADR-0031) via a
  pinned event-kind → message-role fold, **materialized once server-side** and handed to the
  resumed loop as the final state — the client is never asked to re-derive `messages` by
  catching up step-by-step through every intermediate event.
- **New runtime `Resume(loopID)` seam.** On resume it resolves the tenant-scoped loop record
  (ADR-0034 ownership; cross-tenant ⇒ not-found), and — if the loop was reaped/evicted —
  replays events, folds them into the transcript, seeds a fresh agent with it, and **re-parks**
  under a session-scoped ctx so `Send` resumes instead of returning `ErrLoopFinished`. Identity
  flows through the same `LoopContext` seam as `StartLoop` (ADR-0030 preserved).
- **Reuse the existing Connect surface — no new wire RPC.** Client resume is re-opening
  `Observe(loop_id, from_seq=last_seen)` (already history-then-live per ADR-0033) plus `Send`.
  An acceptance test drives that exact client-visible sequence against the real server to prove
  resume ergonomics actually work.
- **Consumes #49's tenancy** (ADR-0034); does not re-litigate identity.

> Reconcile note: this stub was filed against the pre-#55 wire (`ServeWS`, `GET /loops/{id}/events`).
> ADR-0033 (#55) replaced that transport with Connect/protobuf `fuse.loop.v1`; the design decisions
> survive the remap and the spec is written against today's reality. The implementer's just-in-time
> reconcile re-validates against `origin/main`.

## Out of scope

- **Who owns a session / tenant enforcement** — change 0049. This change consumes 0049's
  ownership model (ADR-0034); it does not define it.
- **Per-tenant / configurable idle-TTL policy** — a single named constant lands now; a
  configurable, tenant-aware timeout is a follow-up.
- **New `fuse.loop.v1` RPCs** — explicitly avoided; resume reuses `Observe` + `Send`.
- **Client-SDK session ergonomics** (a versioned external session envelope, resume helpers)
  — change 0050 wraps whatever primitive this change lands.
- **Streaming token deltas** for the restored view — independent (0053 out-of-scope too).

## Note

Filed 2026-08-11 while reconciling 0053 (PR #51); groomed 2026-08-15. 0053's park mechanism is
correct and non-leaking for the connected case; this change lifts the "survives disconnect /
resume by refresh" requirement that 0053 deferred. Persistence model (rebuild from events) and
resume surface (reuse Connect stream) settled in the spec.

## Reconcile log

**2026-08-15** — An earlier `docket-implement-next` run recorded a `## Run halted` on the premise
that subagent dispatch was broken (a control probe appeared to hang). That premise was **false**:
the dispatched agents were completing asynchronously and returning results; a `PONG` control probe
subsequently returned in ~2s, and the reconcile-verification agent returned a full report. The halt
was cleared and the change built to completion.

**Reconcile against `origin/main` (`52f3276`) — all spec assumptions hold.** Verified: the
`inproc.go` run-ctx derivation (`runCtx` descends from the request ctx), the `internal/event.Kind`
constant list, the `model.Message` / tool-call / tool-result shapes, the `Attach`→`Replay` +
Connect `Observe` primitives, and the durable registry/lease mechanics. Dependency `#53` and
related `#47`/`#48`/`#49`/`#50`/`#55` are all `done` — genuinely unblocked. **One open question the
spec flagged was resolved at build time:** user/human input was NOT in the durable event stream
(only appended to the in-memory transcript), so the D5 fold would have dropped every user turn — a
new `KindUserInput` event was added to make the stream a complete transcript source.

**Base-branch drift:** the feature branch is based on `e6e637f` (the merge-base); `origin/main` has
since advanced with the observability work (0051/0061). Nothing in this change conflicts by intent;
the finalize rebase-onto-base gate reconciles at merge time.

Built to open PR **#60**; `reconciled: true`. See the plan and results artifacts for the task
breakdown and the full-suite outcome (including a pre-existing, change-independent flake in
`cmd/fuse` `TestObservabilityAcceptanceHermetic`, reproduced on clean `main`).
