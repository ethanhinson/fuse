---
slug: deglobalize-holder-also-per-instance-the-shared-graph
hook: "Deglobalizing a process-global holder (event store, sink) to per-instance state is only half the job — the dependency graph wired ONCE at construction and shared across instances (a shared tree, scheduler, blackboard, root registry) is itself a second hidden global; a per-instance factory must re-create ALL of it per instance, or a 2nd concurrent instance collides on the shared object even after the named global is gone"
topics: [go, refactoring, concurrency, multi-tenancy, subagents]
changes: [46]
created: 2026-08-10
updated: 2026-08-10
promotion_state: candidate
---

## Apply

When you remove a named process-global (a `currentEventStore()` holder installed via a
`Deps.InstallGlobalStore` hook, an `activeSegmentSink`, etc.) to let one process host N
concurrent instances, the holder is rarely the *only* thing shared. Anything the `Deps`
struct (or equivalent one-time wiring) builds **once at construction** and every instance then
reads — the agent tree, the scheduler, the blackboard, the root tool registry, spawner
closures bound to that one tree — is a second global with the same clobber hazard, invisible
because it has no `set…`/`current…` accessor to grep for.

The fix is structural: turn the per-instance entry point (`StartLoop`) into a **factory** that
re-creates the whole per-instance graph — a fresh tree + store + cloned registry per instance,
sharing nothing — rather than threading only the store through closures that still capture the
one shared tree. Expect the factory signature to grow **beyond** what a plan authored before
this was understood proposed: it must receive/return the per-instance tree and subsume any
child-builder that was itself bound to the old shared tree. Audit the collision by RootID (or
whatever keys instances): if two concurrent starts would produce the same key, something is
still shared.

## War story

- 2026-08-10 (#46, PR #49) — De-globalizing the event-store/segment-sink holders (retiring the
  ADR-0027 `Deps.InstallGlobalStore` bridge) was assumed to be "thread the per-loop store as a
  value instead of `currentEventStore()`." The plan proposed `BuildAgent(store, modelID,
  toolReg)`. But the loop-server's per-loop wiring — scheduler, blackboard, spawners, root tools
  — was all bound to **one shared tree** built at `Deps` construction, so a 2nd `loop.start`
  collided on the same `tree.RootID()` even with the store global removed. Final shape (D-1
  deviation) grew to a per-loop factory `BuildAgent(store, tree, modelID, toolReg) → (agent,
  ChildBuilder, string, error)` that receives the loop's fresh tree and returns its child-builder
  (subsuming the deleted `Deps.BuildChild`); `StartLoop` now creates a fresh tree per loop for the
  loop-server. The three single-loop CLI bindings replaced the package global with a per-`Deps`-
  instance mutex-guarded `eventStoreHolder`. Verified per-loop Seq isolation (each stream starts
  at 1, no cross-loop bleed) and `-race` green across 25 packages with a real deepseek model.
