---
name: live-control-reads-state-at-decision-point
title: A user-facing "live" control must read shared state at the decision point, not snapshot it at construction
promotion_state: candidate
changes: [35, 39]
created: 2026-08-06
updated: 2026-08-06
topics: [architecture, ux, state, permissions]
---

If a control is advertised as switchable mid-session, the code that acts on it must read the shared holder at the moment it decides — not copy the value into a field at construction. Change 0017's D10 shipped Shift+Tab / `/mode` switching, but every `PermissionGate` snapshotted the mode via `WithMode(...)` at build time and `CloneForChild` snapshotted again at spawn, so a flip only took effect on the *next* freshly-built gate. The moment a human reaches for the switch is mid-run, while a live gate and its running children keep asking under the stale mode. 0035 fixed it: `currentMode()` reads the `*SessionMode` holder live, `CloneForChild` propagates the holder, and the snapshot field became a fallback only for holderless callers.

**Why:** "Rebuilt per turn" felt equivalent to "live" on paper, but the user's interaction happens *inside* a turn, exactly where the snapshot is stale. A control the UI says is live but that only applies next-turn reads as broken.

**How to apply:** For any toggle exposed as immediate, trace the read path from the actor to the holder and confirm there is no intervening copy. Test the flip against an *already-constructed* actor (not a fresh one), and for child/forked actors confirm they share the holder by reference. Beware the escalation-valve-style double-count when a live read and an explicit setter both detect the same transition — carry a `lastObserved` ledger. Related: [[cross-deferred-integration-gap]].

## War story

### 2026-08-06 — the agents tab froze the model label at the startup alias (#39, PR #18)

Same snapshot bug, a different surface. Change 0012's agents tab rendered the session model from `Model`/`Label` fields that `NewAgentTree(alias, alias)` copied once from the *startup* alias (`cmd/fuse/shell.go`), while `/model` updated only the live `m.alias` holder (`internal/tui/shell_model.go`) — so the tab kept showing the initial model regardless of what the user had switched to. Exactly the 0035 shape (a "live" control snapshotted at construction), one layer up in the TUI instead of the permission gate. The fix followed the same rule: render from the live session alias rather than the tree root's construction-time copy. Notably the *sibling* bug in the same change was the mirror image — the elapsed timer read live state (`StartedAt` stamped at construction) when it should have stayed *zero* until the first `BeginTurn()`; "read state at the decision point" cuts both ways, and the decision point for an idle timer is "has a turn started yet," not "now."
