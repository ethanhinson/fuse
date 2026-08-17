---
id: 68
slug: auto-mode-deterministic-freedom
title: Auto-mode deterministic freedom — scratchpad + write_roots + rules-layer shrink to catastrophic-only
status: done
priority: high
type: feat
created: 2026-08-17
updated: 2026-08-17
depends_on: [67]
related: [40, 57, 63, 64, 67, 69, 70]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-17-auto-mode-deterministic-freedom-design.md
plan: docs/superpowers/plans/2026-08-17-auto-mode-deterministic-freedom-plan.md
results: docs/results/2026-08-17-auto-mode-deterministic-freedom-results.md
trivial: false
auto_groomable:
branch: feat/auto-mode-deterministic-freedom
claimed_at: 
pr: 72
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-17-auto-mode-deterministic-freedom-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-17-auto-mode-deterministic-freedom-design.md) |
| Plan | [2026-08-17-auto-mode-deterministic-freedom-plan.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/2026-08-17-auto-mode-deterministic-freedom-plan.md) |
| Results | [2026-08-17-auto-mode-deterministic-freedom-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-08-17-auto-mode-deterministic-freedom-results.md) |
| PR | 72 |
<!-- docket:artifacts:end -->

## Why

Stage B of the auto-mode overhaul — the biggest observed-pain win. The rules layer terminal-denies routine dev operations: `curl` was 39 of 62 rules-layer denies in real sessions (14+ targeting localhost — agents probing their own dev servers), `kill`/`pkill` another 4, and `/tmp` usage drove most bash classifier denies. Claude Code / Cursor never hard-block these. Terminal denies shrink to catastrophic-only (`mkfs`, `shutdown`/`reboot`, `sudoedit`, `rm -rf` on `/`/`$HOME`/workspace-ancestors, `dd of=/dev/*`); everything else becomes deterministic scoped-allow where provable (loopback curl, single-PID kill, in-workspace rm, scratchpad writes), else context-aware classifier, else ask.

## What changes

- Config: `permissions.auto.allow_push`, `permissions.auto.write_roots` (trusted-source-only; loader presence-predicate extended).
- Per-session scratchpad `~/.fuse/tmp/<session-id>/` treated as workspace-equivalent by all path scoping, advertised in the system prompt; `write_roots` for extra roots (`/tmp` opt-in).
- `dangerousNames` shrink + shape-based catastrophic denies; `kill <pid>` deterministic allow; `curl`/`wget` all-loopback deterministic allow, non-loopback → classifier; `git reset`/`clean` freed; `git push` allow-by-config else classifier-routed egress.

## Out of scope

- Classifier prompt bias and web_fetch seed promotion (#0069).
- Shell-parse widening — opaque args, redirects, control flow (#0070).
- Container-level egress enforcement (#0063/#0064 — complementary backstop, not a dependency).

## Open questions

<!-- none — spec is settled from the approved plan -->

## Reconcile log

- 2026-08-17 — claimed same day the spec was authored; dependency #0067 merged to main (PR #71, 4fca89b) so the depends_on gate is satisfied. Spec anchors verified during design against the tree #0067 branched from; #0067 touched gate.go/heuristics-adjacent code, so the build re-verifies line anchors against current main before editing. Building inline in the main session; human authorized full merge+finalize ("merge and finalize").
