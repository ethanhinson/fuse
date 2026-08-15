---
id: 54
slug: durable-resumable-sessions
title: Durable, resumable sessions — a conversation survives disconnect; refresh restores transcript + memory
status: in-progress
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-15
depends_on: [53]
related: [47, 48, 49, 50]
discovered_from: [53]
adrs: []
spec: docs/superpowers/specs/2026-08-15-durable-resumable-sessions-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/durable-resumable-sessions
claimed_at: 2026-08-15T05:09:50Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-15-durable-resumable-sessions-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-durable-resumable-sessions-design.md) |
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

## Run halted

**2026-08-15** — `docket-implement-next` claimed this change and then halted before the build,
at Step 3 (reconcile). The change itself is fine; the **harness could not dispatch subagents**.

**What stopped the run.** Every subagent dispatch launched asynchronously and then never
returned a result to the parent. Three attempts, all failed:

1. The Step-0 `docket-status` sweep dispatch — transcript went idle for 20+ minutes with no
   result record.
2. A read-only reconcile exploration of `origin/main` — stopped writing after 104 transcript
   lines, idle 8+ minutes, no result record.
3. A deliberately trivial control probe (`Reply with exactly the word: PONG`, no tools) — 4
   transcript lines, no result after 2 minutes.

The control probe is the decisive one: a no-tool, one-word reply cannot legitimately take
minutes, so this is a dispatch-delivery failure in the harness, not slow subagent work. This
satisfies the convention's *Dispatch-capability resolution* bar — a mechanism was resolved and
attempted, and the attempts failed — rather than inferring absence from a missing tool name.

**Why that halts rather than degrades.** The resolved build role is `skills.build: docket-build`
(not the `auto` sentinel), and `docket-build` implements the build by routing each plan task to a
profile subagent — precisely the capability that is broken. Under the convention's Tier C
(*discipline*) posture, only an explicitly configured `auto` is the human's authorization to run
the build inline; any other resolved value that cannot dispatch is abort-and-report. The same
applies to `skills.review: docket-review`. So this run stopped rather than quietly building the
change inline without that authorization.

**State left behind.** `status: in-progress` with `claimed_at` refreshed; no feature branch was
cut, no worktree created, no plan written, and no code touched. Reconcile did not complete
(`reconciled: false`), so a later run re-reconciles from scratch. Nothing needs undoing.

**What a human must decide.** One of:

- Fix subagent dispatch in the harness and re-run `docket-implement-next` — the change re-claims
  cleanly and this section is removed automatically by the Step-2 claim.
- Or authorize inline execution by setting `skills.build: auto` (and `skills.review: auto`) — note
  this is machine-scoped-able via `.docket.local.yml`, since neither is a coordination-fenced key
  — which trades docket-build's per-task profile routing for a single inline builder.
- Or clear the claim: `docket.sh reclaim-claims` self-heals it back to `proposed` once the
  72-hour lease expires (no feature branch exists, so it is auto-reclaimable).

**Reconcile findings gathered before the halt** (useful to whoever picks this up): dependency
`#53` is `done`, and related `#47`, `#48`, `#49`, `#50`, `#55` are all `done`, so the change is
genuinely unblocked; the spec at `docs/superpowers/specs/2026-08-15-durable-resumable-sessions-design.md`
was authored 2026-08-15 against the post-ADR-0033 Connect wire and is current. The spec's
code-level assumptions (`inproc.go` line references, the `internal/event.Kind` constant list, the
live tool-call/tool-result message shape) were **not** re-verified against `origin/main`
(`52f3276`) — that was the exploration that died — and remain open for the next reconcile.
