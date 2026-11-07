---
id: 150
slug: 'per-model-profiles-for-prompt-edit-tool-and-effort'
title: 'Per-model profiles for prompt, edit tool and effort'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-07'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec:
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

Model control is the reason to own this harness, and real runs swing widely on these settings: GLM made no edits in 19 minutes at default effort and committed in 2 at medium. Today they are scattered and mostly unset.

## What changes

- One place per model for persona additions, edit tool shape, reasoning effort, token limits and provider routing hints.
- Ship tested defaults only for models with evidence behind them.
- Tests cover merging and defaults.

## Out of scope

Automatic tuning. Defaults must not change because of a benchmark alone.
