---
id: 69
slug: auto-mode-classifier-retune-webfetch
title: Auto-mode classifier retune + web_fetch loosening — allow-bias for routine dev ops, seed becomes real auto-approve
status: in-progress
priority: medium
type: feat
created: 2026-08-17
updated: 2026-08-18
depends_on: [68]
related: [40, 45, 67, 68, 70]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-17-auto-mode-classifier-retune-webfetch-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/auto-mode-classifier-retune-webfetch
claimed_at: 2026-08-18T03:50:20Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-17-auto-mode-classifier-retune-webfetch-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-17-auto-mode-classifier-retune-webfetch-design.md) |
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
