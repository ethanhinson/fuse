---
id: 142
slug: 'compound-shell-commands-reach-the-auto-mode-classifier-with'
title: 'Compound shell commands: parse-layer decomposition with per-part deterministic evaluation'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-05'
updated: '2026-11-05'
depends_on: [70]
stacked_on:
related: [67, 68, 69]
discovered_from: []
adrs: [5, 6]
spec: 'docs/superpowers/specs/2026-11-05-compound-shell-commands-reach-the-auto-mode-classifier-with-design.md'
plan: 'docs/superpowers/plans/2026-11-05-compound-shell-commands-reach-the-auto-mode-classifier-with-plan.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/compound-shell-commands-reach-the-auto-mode-classifier-with'
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-05-compound-shell-commands-reach-the-auto-mode-classifier-with-design.md](../../superpowers/specs/2026-11-05-compound-shell-commands-reach-the-auto-mode-classifier-with-design.md) |
| Plan | [2026-11-05-compound-shell-commands-reach-the-auto-mode-classifier-with-plan.md](../../superpowers/plans/2026-11-05-compound-shell-commands-reach-the-auto-mode-classifier-with-plan.md) |
| ADRs | [ADR-0005](../../adrs/0005-per-segment-allow-rule-evaluation.md), [ADR-0006](../../adrs/0006-fuse-local-yml-tighten-only-trust-boundary.md) |
<!-- docket:artifacts:end -->

## Why

In auto mode, compound shell commands — `make test && git status`, `cat foo | grep bar`, `$(git rev-parse HEAD)` — are the single biggest source of human approval interrupts. The gate's shell parser cannot prove a compound command safe first-token-at-a-time, so it falls through the deterministic layers and reaches the LLM classifier as one opaque raw string. The classifier, seeing no structure, either verdicts conservatively (deny/ask) or the command bounces straight to the y/N/a human prompt. The result: the user shepherds every multi-step command by hand, which is exactly the prompt fatigue auto mode exists to eliminate. Change 0070 widened shell-parse but deliberately left the classifier-facing contract alone; the decomposition exists at the parse layer today but is discarded before the classifier sees it.

## What changes

When the gate's parser decomposes a compound command into parts (pipeline stages, && / || / ; operands, command substitutions), the auto-mode layer hands the classifier the full structure — the raw command plus the parsed parts, each with its own first-token classification and any per-part deterministic verdict already reached — instead of only the raw string. The classifier prompt is extended to verdict each part and the composition; a compound is auto-approved only when every part is independently safe and the composition introduces no new risk (e.g. a write piped into a shell). Commands with any unclassifiable or unsafe part keep today's behavior: gray-area parts route to the human ask, unsafe parts deny. The audit record for a compound decision carries the per-part verdicts so `LayerClassifier` outcomes stay attributable. Deterministic catastrophic-deny and the permission floor are unchanged and still evaluated before the classifier is consulted.

## Out of scope

Widening the deterministic scoped-allow rules themselves (new provable-safe command shapes — that is a 0068-style follow-up). Session-scoped or remembered approvals ('always allow this shape') — a separate grant-model change. Retuning the classifier's risk bias beyond passing the new structural context (0069 owns the base prompt contract). Changes to smart / prompt-all / off modes, the permission floor, or the human prompt UX itself.
