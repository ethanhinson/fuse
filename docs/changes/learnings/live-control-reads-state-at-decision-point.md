---
name: live-control-reads-state-at-decision-point
title: A user-facing "live" control must read shared state at the decision point, not snapshot it at construction
promotion_state: candidate
changes: [35]
created: 2026-08-06
updated: 2026-08-06
topics: [architecture, ux, state, permissions]
---

If a control is advertised as switchable mid-session, the code that acts on it must read the shared holder at the moment it decides — not copy the value into a field at construction. Change 0017's D10 shipped Shift+Tab / `/mode` switching, but every `PermissionGate` snapshotted the mode via `WithMode(...)` at build time and `CloneForChild` snapshotted again at spawn, so a flip only took effect on the *next* freshly-built gate. The moment a human reaches for the switch is mid-run, while a live gate and its running children keep asking under the stale mode. 0035 fixed it: `currentMode()` reads the `*SessionMode` holder live, `CloneForChild` propagates the holder, and the snapshot field became a fallback only for holderless callers.

**Why:** "Rebuilt per turn" felt equivalent to "live" on paper, but the user's interaction happens *inside* a turn, exactly where the snapshot is stale. A control the UI says is live but that only applies next-turn reads as broken.

**How to apply:** For any toggle exposed as immediate, trace the read path from the actor to the holder and confirm there is no intervening copy. Test the flip against an *already-constructed* actor (not a fresh one), and for child/forked actors confirm they share the holder by reference. Beware the escalation-valve-style double-count when a live read and an explicit setter both detect the same transition — carry a `lastObserved` ledger. Related: [[cross-deferred-integration-gap]].
