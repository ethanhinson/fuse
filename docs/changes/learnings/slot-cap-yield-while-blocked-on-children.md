---
name: slot-cap-yield-while-blocked-on-children
slug: slot-cap-yield-while-blocked-on-children
title: A concurrency cap that counts blocked parents deadlocks — yield the slot while waiting on children
hook: "Bounded-concurrency caps must not charge a holder that is blocked waiting on other holders — release (yield) the slot while waiting, reacquire after, or nested work deadlocks the pool"
promotion_state: candidate
changes: [12]
created: 2026-08-05
updated: 2026-08-05
topics: [concurrency, deadlock, semaphores, goroutines, subagents]
---

When a semaphore/width cap bounds "how many workers exist" and a worker can itself *spawn and wait on* more capped workers, the cap deadlocks: N parents each hold a slot while blocked in the spawn call, their children queue behind the full cap, and nothing ever finishes. The cap must count **active** work, not **alive** holders.

**Rule:** in any bounded pool where a holder can block on other members of the same pool:
- Release the holder's slot when it blocks on children (`YieldSlot`), reacquire on resume (`UnyieldSlot`).
- Refcount the yield if a holder can wait on several parallel batches.
- Exempt the root/top-level caller (it never queues behind its own descendants).
- Write a regression test that reproduces the freeze shape: fill the cap entirely with parents that each spawn one child, and assert completion.

**Why it's easy to miss:** the single-level case works perfectly — the deadlock only appears when spawn depth ≥ 2 *and* enough concurrent parents saturate the cap, which unit tests rarely construct. It surfaces live as a total freeze with no error.

The same shape hides in a second place: a **single mutable slot** used for concurrent request/response handshakes (0012's approval slot). N concurrent requests each overwrote the slot, orphaning the previous responder channels; goroutines blocked forever on `<-respCh` and the turn froze. Same fix family: replace the slot with a FIFO queue owned by exactly one component, and drain-with-deny at scope end so no waiter is ever orphaned.

## War story

(#12, PR #10) — Fuse subagent runtime. `MaxConcurrentSpawns = 8` counted agents alive; 8 parents blocked in `spawn_agent` held every slot while their children queued — observed live as a fully frozen agent tree. Fixed with refcounted `AgentTree.YieldSlot`/`UnyieldSlot` (root exempt) plus a regression test reproducing the freeze. Separately, the single `m.approval` slot orphaned responder channels under parallel spawns (trace4 total freeze); replaced by an approval FIFO queue single-homed in `ShellModel`, drained with deny at turn end.
