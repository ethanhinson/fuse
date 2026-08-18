---
id: 69
slug: auto-mode-classifier-retune-webfetch
title: Auto-mode classifier retune + web_fetch loosening — allow-bias for routine dev ops, seed becomes real auto-approve
status: implemented
priority: medium
type: feat
created: 2026-08-17
updated: 2026-08-18
depends_on: [68]
related: [40, 45, 67, 68, 70]
discovered_from: []
adrs: [48]
spec: docs/superpowers/specs/2026-08-17-auto-mode-classifier-retune-webfetch-design.md
plan: docs/superpowers/plans/2026-08-18-auto-mode-classifier-retune-webfetch-plan.md
results: docs/results/2026-08-18-auto-mode-classifier-retune-webfetch-results.md
trivial: false
auto_groomable:
branch: feat/auto-mode-classifier-retune-webfetch
claimed_at: 2026-08-18T05:32:00Z
pr: https://github.com/ethanhinson/fuse/pull/75
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-17-auto-mode-classifier-retune-webfetch-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-17-auto-mode-classifier-retune-webfetch-design.md) |
| Plan | [2026-08-18-auto-mode-classifier-retune-webfetch-plan.md](https://github.com/ethanhinson/fuse/blob/feat/auto-mode-classifier-retune-webfetch/docs/superpowers/plans/2026-08-18-auto-mode-classifier-retune-webfetch-plan.md) |
| Results | [2026-08-18-auto-mode-classifier-retune-webfetch-results.md](https://github.com/ethanhinson/fuse/blob/feat/auto-mode-classifier-retune-webfetch/docs/results/2026-08-18-auto-mode-classifier-retune-webfetch-results.md) |
| PR | [#75](https://github.com/ethanhinson/fuse/pull/75) |
| ADRs | [ADR-0048](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0048-web-fetch-host-floor-as-authorization-boundary.md) |
<!-- docket:artifacts:end -->

## Why

Stage C of the auto-mode overhaul. Both LLM classifier prompts are explicitly block-biased, and the web_fetch known-good seed is only a bias hint — every fetch still runs the classifier. Real sessions show 32 web_fetch classifier denies of ordinary hosts (web.archive.org, hn.algolia.com, lmstudio.ai, duckduckgo, bing, google). With #0068's catastrophic floor in place, safety no longer depends on classifier pessimism: the prompts can say what Claude Code / Cursor embody — routine dev operations and public web reads are default-allow; deny only named dangerous shapes.

## What changes

- Bash classifier system prompt rewritten allow-biased for routine dev ops; workspace root + scratch dir added to the pending-call prompt.
- web_fetch: exact/suffix seed hosts + reputation top-sites become real auto-approve (`DecidedBy:"known-good"`, zero classifier calls); TLD wildcards stay nudge-only; config `fetch_deny`/`fetch_ask` still beat the seed; remaining hosts get an allow-biased GET prompt with named deny shapes.
- Valve total 20 → 50 (consecutive stays 3), driven off the constant in tests.

## Out of scope

- Rules/heuristic layer changes (#0068). Shell parsing (#0070). Reputation DB expansion beyond what #0045 shipped.

## Open questions

<!-- none — spec is settled from the approved plan -->

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-18

Reconciled against `origin/main` @ `0257678` and the metadata branch.

**Verdict: spec holds unchanged.** Every design element (D1/D2/D3) maps onto code that still
exists in the shape the spec describes. Adjustments are line references and one plumbing detail:

- `classifierSystemPrompt` is still the block-biased 6-line const (`classifier.go:54-59` — exact
  match). `webFetchPendingPrompt` is at `classifier.go:240-253` (spec said 244-253).
  `cloneForChild` at `classifier.go:137-151` — copies `client`/`modelID`/`cache` only, so the two
  new context fields must be added there as the spec requires.
- `knownGoodSeed` (`fetchhost.go:26-60`) and `classifyFetchHost` (`fetchhost.go:130-159`) are
  byte-for-byte where the spec left them; the floor order (SSRF → config deny → config ask →
  blocklist → fallthrough) is unchanged, so "config always beats the seed" needs no reordering.
- The web_fetch routing block the spec cited as `gate.go:449-459` now sits at **`gate.go:592-607`**
  (drift from #0067/#0068 landing). Same `switch r.Verdict` shape; the `known-good` allow arm goes
  in its `default:` branch before `g.classifyWebFetch`.
- Valve constants moved from the cited `gate.go:106-109` to **`gate.go:120-126`**
  (`valveConsecutiveLimit = 3`, `valveTotalLimit = 20`). `valve_test.go:243-269`
  (`TestValve_TwentyTotalBlocks_TripsIndependentOfConsecutive`) hardcodes 20/40/39 literals — the
  spec's "drive tests off the constant" is a real edit, plus its name must lose the "Twenty".
- **Plumbing nuance:** the spec says the classifier's workspace/scratch fields are "set from
  `autoModeOptions`" (`cmd/fuse/run.go:387`). `autoModeOptions` already has `workspaceRoot()` in
  scope, but the scratch dir is resolved one level up in `buildGate` via `gateWriteRoots(cfg)`
  (`cmd/fuse/scratch.go:74`). The classifier is constructed inside `autoModeOptions`, so that
  function will call `sessionScratchDir()` directly rather than the caller threading it down — no
  signature churn, same value, spec intent preserved.
- Dependency **#0068 is `done`** (archived 2026-08-17), so #0068's catastrophic floor — the safety
  argument this change leans on for dropping classifier pessimism — is in fact merged. #0070
  (shell-parse widening) is still `proposed` and correctly out of scope.

No scope dropped, nothing already done elsewhere, no new constraints. Auto-capture is disabled for
this repo, so no follow-up stubs were minted.
