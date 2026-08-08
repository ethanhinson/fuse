<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0023 — Shared result blackboard for inter-agent communication](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0023-agent-blackboard.md)**
<!-- docket:backlink:end -->

# Shared result blackboard — results

Change: #0023 · Branch: feat/agent-blackboard · PR: <set at PR open> · Plan: docs/superpowers/plans/0023-agent-blackboard.md · ADRs: none

## Verify (human)

Automated tests cover the store, tool, wiring, and TUI render. The one surface
worth a live look at the merge gate:

- [ ] Blackboard tab renders live in the `/agents` overlay — run the interactive
      shell, spawn a couple of agents that `blackboard_write`, open the agents
      overlay and press `b`; confirm keys, JSON values, and the
      `⟨written by <label>⟩` provenance indicators show and stay readable when a
      value is large or contains control bytes.
- [ ] (optional) Producer/consumer smoke: one agent `blackboard_wait`s on a key a
      sibling writes; confirm the waiter wakes and the pool does not freeze under
      a saturated concurrency cap (the deadlock shape from 0012). Covered by
      `TestWaitSlotYieldSaturationRegression`, but a live run is the ultimate check.

## Findings

- **Import-cycle seam (design decision, no ADR).** `internal/agent` imports
  `internal/tools`, so the tool cannot reference the store type directly. The
  `blackboard` tool reaches the store through a narrow `tools.BlackboardStore`
  interface implemented by a per-node handle (`agent.Blackboard.ForNode(node)`),
  exactly mirroring how `SpawnFunc` breaks the spawn cycle. This keeps provenance
  (writer node id/label) machine-stamped and out of the model's hands. Judged an
  application of an existing accepted pattern rather than novel architecture, so
  no ADR was minted; documented in the spec, the reconcile log, and inline.
- **Spec wiring-site drift, corrected at reconcile.** The spec named the wiring
  sites as `run.go`/`shell.go`/`research_probe.go` and flagged `workflow.go` to
  re-grep. The authoritative grep (and learning `patch-every-cloned-child-builder`)
  shows the three agent entry points are **`main.go` (one-shot run), `shell.go`,
  `research_probe.go`**; `run.go` holds only shared registry helpers and
  `workflow.go` only budget/quota helpers. Wired all three; recorded in the
  reconcile log.
- **Slot-yield reuse.** `blackboard_wait` yields via the existing
  `AgentTree.YieldSlot`/`UnyieldSlot` (0012), which are no-ops for the depth-0
  root — the saturation regression therefore uses depth-≥1 producer/consumer
  nodes.
- **Review verdict:** no blockers; concurrency (lost-wakeup guard, signal-under-
  lock, yield on every return path), tool-error discipline, and wiring all
  confirmed correct. The review's one should-fix (wiring tests reconstructed the
  registry rather than proving each entry point calls the wiring) was addressed:
  extracted a shared `wireRootBlackboard` helper and added a structural
  regression test (`TestEveryAgentEntryPointWiresBlackboard`) that greps the
  `WithChildBuilder` entry-point sources and asserts each calls both wiring
  helpers.

## Follow-ups

Documented non-goals for later changes (all confirmed out of scope here):

- ACLs / per-agent key namespacing.
- Smarter wait liveness beyond timeout (tree-idle / producer-death wake).
- Value size accounting against the context budget.
- Real mid-run message injection (the directed-message inbox here is poll-based).
- **Pre-existing observation (not introduced here):** on the `ctx.Done()` path of
  a yielded wait, the deferred `UnyieldSlot` runs with an already-cancelled ctx so
  slot accounting can transiently under-count (clamped at 0, never negative). This
  is the existing 0012 yield/unyield behavior reused verbatim, not a new defect —
  flagged for awareness, a candidate for a future scheduler-accounting change.
- #0026 (workflow composition) and the folded-in directed-message convention build
  on this substrate.
