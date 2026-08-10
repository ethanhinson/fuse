<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0046 — De-globalize the event store + multi-loop host — one process hosts N loops keyed by loop_id](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0046-multi-loop-host-deglobalize-event-store.md)**
<!-- docket:backlink:end -->

# Spec 0046 — De-globalize the event store + multi-loop host: one process hosts N loops keyed by loop_id

## Problem

The Runtime-seam trilogy (0043 typed event stream, 0044 handle-returning spawn, 0045 named
`Runtime` interface + `fuse loop-server` binding) delivered a policy-free `Runtime` — but it
landed on an implementation that is **structurally single-loop-per-process**. Two coupled
process-globals keep it that way, and both were consciously deferred by 0045 (ADR-0027, ADR-0025):

1. **Process-global event-store + segment-sink holders (the ADR-0027 bridge).** Although the
   in-process Runtime (`internal/runtime.inProcRuntime`) already owns each loop's `fsstore`
   event store as instance state keyed by `tree.RootID()`, it still installs that store into a
   **process-global, RW-mutex-guarded holder** (`setActiveEventStore`/`currentEventStore`, and the
   parallel `setActiveSegmentSink`/`currentSegmentSink`) via the `Deps.InstallGlobalStore` hook.
   The child-builder closures in `cmd/fuse` read the store back through `currentEventStore()` —
   a single global slot. A second concurrent loop calling `StartLoop` **clobbers** that slot; the
   first loop's child-builder closures then read the second loop's store. There is exactly one
   live global at a time, so genuinely concurrent hosting is impossible without cross-loop bleed.

2. **Per-session-global Seq allocation (ADR-0025).** `Seq` is allocated inside the `fsstore`
   under its own lock (`s.seq++; e.Seq = s.seq`), which gives a clean single total order and a
   `Replay(from Seq)` cursor — but ADR-0025 explicitly assumes **one process ⇒ one session**
   (ADR-0019) and its own Consequences say a multi-session-per-process store "would revisit Seq
   allocation." Each loop already gets its own `fsstore` instance, so Seq *is* per-store today;
   the risk is any remaining process-global assumption in how Seq origin/monotonicity is reasoned
   about across loops, and the need to prove there is **no cross-loop Seq bleed** once N stores
   are live simultaneously.

A hosted fuse service — the chosen deployment shape (fuse as a standalone hosted service; the
user's other apps are thin network clients over it) — is **inherently N concurrent loops per
process**. So these two globals must become **per-loop instance state** before *any* networked or
multi-tenant hosting is possible. This change is the **keystone** that unlocks the first two
hosting milestones the user cares about: (1) a remote loop over the network, and (3) durable /
distributed persistence. It is the last purely-internal, single-tenant, in-process step — no
transport and no auth are introduced here (those are downstream arc changes C and D).

ADR-0027 and ADR-0025 both name this change as their required follow-up. ADR-0027's Consequences:
*"A follow-up change is required to de-globalize the holders and revisit Seq allocation before a
loop-server can host multiple concurrent loops."* This is that change.

## Goals

- Move **event-store and segment-sink ownership** from process-global holders to **per-loop
  instance state** on the in-process Runtime implementation, so N loops can be live in one process
  with **no shared-global contention and no cross-loop bleed**.
- Make `loop.start` in `fuse loop-server` support **N live loops keyed by loop_id**
  (`tree.RootID()`) in a single process, where today the process caps at one live loop.
- Guarantee **per-loop Seq allocation** with a provably isolated event stream per loop (no
  cross-loop Seq bleed; each loop's `Replay(from Seq)` cursor addresses only its own history).
- Keep existing **single-loop behavior byte-identical** (one-shot CLI, single interactive shell,
  single-loop loop-server all unchanged), and keep the whole change **in-process and
  single-tenant** — no transport, no auth.
- **Revisit / supersede ADR-0027 and ADR-0025** as the implementation requires (the bridge
  becomes unnecessary; Seq's one-process-one-session premise is retired).

## Non-goals (explicit — belong to later arc changes)

- **No network transport.** Still in-process Go calls. WS/HTTP bindings over the seam are change C.
- **No durable / distributed event store.** Per-session JSONL files stay the persistence; surviving
  process restart and sharing across instances is change B.
- **No auth / multi-tenancy.** loop_id ownership and per-tenant isolation are change D — this change
  stays single-tenant (all loops in the process share one trust domain, as the loop-server does today).
- **No client SDK.** Change E.
- **No `Runtime` interface change.** The seam (`StartLoop`/`Send`/`Spawn`/`Observe`/`Attach` keyed
  by `tree.RootID()`) is already designed for N loops; this change makes the *implementation* honor
  that shape. If a signature must change it is a deliberate, called-out exception, not a goal.

## Design sketch (to be finalized during build reconcile)

The 0045 implementation already keyed loops in a `loops map[string]*loop` on `inProcRuntime`, so
the interface shape is correct — the work is removing the two globals that leak underneath it.

**1. De-globalize the event-store / segment-sink read path.** The leak is not the *write* side
(each loop already owns its `fsstore`); it is the **read** side — the `cmd/fuse` child-builder
closures resolving the store through `currentEventStore()` / `currentSegmentSink()` process-globals.
The fix is to thread the **owning loop's store/sink instance** to the code that currently reaches
for the global, so the resolution is per-loop rather than per-process. Candidate shape: the
child-builder factory is parameterized by (or closes over) the loop's store/sink at `StartLoop`
time instead of calling the global accessor, so each loop's agents and spawned children resolve
*their own* loop's store. The `Deps.InstallGlobalStore` bridge hook (ADR-0027) is then removed or
reduced to a no-op back-compat shim; `internal/runtime` continues to never import `cmd/fuse`.
Both the event-store holder and the segment-sink holder are migrated together — they are the same
pattern and the same hazard.

**2. Per-loop Seq allocation, proven isolated.** Confirm each loop's `fsstore` is the sole Seq
allocator for its own stream (it already increments under its own lock), and remove or rewrite any
reasoning/comment/assertion that presumes one-session-per-process. The verification bar is a test
with **N concurrent loops each Appending** and an assertion that each loop's `Replay` returns a
contiguous, self-consistent Seq run with **no events from another loop** — i.e., no cross-loop Seq
bleed and no shared counter.

**3. Multi-loop `loop.start` in `fuse loop-server`.** `loop.start` registers a new loop in the
Runtime's `loops` map keyed by the returned loop_id and returns that id; `loop.send`, `loop.observe`,
and replay all dispatch by loop_id to the correct per-loop store. Multiple `loop.start` calls in one
process yield multiple concurrently-live, independently-observable loops. Lifecycle: a loop is
removed from the map / its store closed when its run goroutine completes (as 0045 already does for
the single loop), now correct for N.

**4. ADR bookkeeping.** ADR-0027 (the global-holder bridge) is **superseded** — the bridge it
described is retired. ADR-0025 is **revisited** — either superseded or amended with a dated
`## Update` — to record that Seq is per-loop-store and the one-process-one-session premise no longer
holds. New ADR(s) record the de-globalization decision and the multi-loop hosting model. Exact ADR
actions are settled at build time by `docket-adr`.

## Verification

- **N concurrent loops, isolated streams.** A test starts N (≥2, ideally ≥3) loops in one
  loop-server process, drives events into each, and asserts every loop has an **isolated event
  stream**: no cross-loop Seq bleed, each `Replay(from Seq)` returns only that loop's events, and no
  two loops contend on a shared global (the old single-slot holder is gone).
- **No shared-global contention.** Assert (structurally / by construction, and by a concurrency
  test) that starting a second loop does not disturb the first loop's store or sink resolution — the
  clobber hazard ADR-0027 documented is eliminated.
- **Single-loop behavior unchanged.** Existing one-shot CLI, interactive shell, research probe, and
  single-loop loop-server behavior are byte-identical / test-unchanged; one-shot still uses
  `NoopStore` when `BaseDir == ""`.
- **-race green.** `make test-race` (`go test -race ./...`) passes, including the new N-loop
  concurrency test — the load-bearing gate, since the whole change is about removing shared mutable
  process state.

## Dependencies

- **Depends on 0045** (`runtime-interface-and-binding`, PR #48) — this change edits the
  `internal/runtime` seam and the `fuse loop-server` binding that 0045 introduced, and retires the
  ADR-0027 bridge / ADR-0025 premise that 0045 deliberately left in place.

## Open questions (resolve at build reconcile)

- **Bridge removal vs. shim.** Can `Deps.InstallGlobalStore` and the `currentEventStore()` /
  `currentSegmentSink()` accessors be deleted outright, or must a no-op shim remain for an
  out-of-tree reader? Lean: delete if no reader survives; keep a documented no-op only if one does.
- **How the child-builder closures receive the per-loop store.** Constructor parameter, context
  value, or a per-loop factory closure — pick the one that keeps `internal/runtime` free of any
  `cmd/fuse` import and reads cleanest. Lean: per-loop factory closure created at `StartLoop`.
- **ADR-0025 action: supersede vs. amend.** If Seq is already effectively per-store, an amendment
  (dated `## Update`) may suffice; if the allocation model materially changes, supersede. Decide when
  the code is written.
- **loop_id collision / lifecycle in the map.** Confirm `tree.RootID()` uniqueness across concurrent
  loops in one process and define behavior for `loop.start` when a loop_id would collide (should not
  happen, but assert it) and for `loop.send`/`observe` to an unknown or already-finished loop_id.
