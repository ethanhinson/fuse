<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0046 — De-globalize the event store + multi-loop host — one process hosts N loops keyed by loop_id](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0046-multi-loop-host-deglobalize-event-store.md)**
<!-- docket:backlink:end -->

# De-globalize the event store + multi-loop host — results
Change: #46 · Branch: feat/multi-loop-host-deglobalize-event-store · PR: (opened at close-out) · Plan: docs/superpowers/plans/2026-08-10-multi-loop-host-deglobalize-event-store.md · ADRs: 25 (amended), 27 (superseded), 30 (new)

## Verify (human)

Automated tests fully cover the change; no manual gate checks are strictly required. Optional confidence checks:
- [ ] `go test -race ./...` locally is green (the load-bearing gate — CI runs it too).
- [ ] `go test -race -count=3 -run 'MultiLoop|multiloop' ./internal/runtime/... ./cmd/fuse/...` stays green (concurrent isolation, no flakes).

## Findings

- **The seam was already N-loop-shaped; only the cmd-layer read path was global.** `internal/runtime`
  already owned per-loop stores in `loops map[string]*loop`; the loop-server `loop.start` already
  registered/returned loop_ids. The single blocking global was the four `cmd/fuse` Deps-builders
  resolving the store back through the process-global `currentEventStore()` (installed via
  `Deps.InstallGlobalStore` at StartLoop), which a 2nd concurrent loop clobbered.
- **ADR-0030 (new, supersedes ADR-0027):** the process-global event-store + segment-sink holders and
  `Deps.InstallGlobalStore`/`Deps.BuildChild` are deleted; the per-loop store flows as a value; the
  loop-server creates a fresh tree + fsstore + cloned tool registry per StartLoop (shares nothing);
  the three single-loop CLI bindings use a per-Deps-**instance** `eventStoreHolder` (mutex-guarded,
  instance-scoped) replacing the package global.
- **ADR-0025 (amended, dated `## Update`):** the one-process-one-session premise is retired. The Seq
  allocation model is unchanged (per-store allocator under its own lock, sole allocator, single total
  order, clean `Replay(from Seq)`); only the *scope* of "the store" changed from per-process to
  per-loop. Provably no cross-loop Seq bleed — each loop's stream starts at 1. No supersession needed.
- **Whole-branch review: mergeable, no blocking issues.** Two non-blocking notes: M1 a stale
  `Deps.BuildChild` comment (fixed in commit d78ff1a), and N1 a defensive dead branch in the shell's
  `BuildAgent` re-seed (left as documented follow-up — harmless; the shell discards `runtime.New` and
  never calls StartLoop).

## Plan deviations

- **D-1 — `Deps.BuildAgent` signature grew beyond the plan's stated shape.** The plan proposed
  `BuildAgent func(store, modelID, toolReg) (...)`. Final shape is
  `func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry)
  (*agent.Agent, agent.ChildBuilder, string, error)` — a **per-loop factory** that also receives the
  loop's fresh **tree** and **returns the loop's child-builder** (subsuming the removed
  `Deps.BuildChild`). This was necessary because the loop-server's per-loop wiring (scheduler,
  blackboard, spawners, root tools) was all bound to one shared tree built at Deps-construction, so a
  2nd `loop.start` collided on the same RootID. StartLoop now creates a fresh tree per loop for the
  loop-server (Deps.Tree unset). A deliberate, called-out exception (spec non-goal #5).

## Follow-ups

- **N1 (nice-to-have):** if the shell ever adopts StartLoop-driven turns with `BaseDir` set, the root
  (which emits to the shell-owned `eventStore` directly) and children (which read the re-seeded
  `storeHolder`) could diverge onto two fsstores. Not a current bug; worth a one-line caution comment
  or a small refactor if/when the shell moves to StartLoop. Not filed as a change (auto-capture is
  disabled in this repo); noted here for the record.
