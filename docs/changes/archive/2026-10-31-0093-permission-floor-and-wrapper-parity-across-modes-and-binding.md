---
id: 93
slug: 'permission-floor-and-wrapper-parity-across-modes-and-binding'
title: 'Permission floor and wrapper parity across modes and bindings'
status: 'done'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: [92]
stacked_on:
related: [87, 88, 92]
discovered_from: [88]
adrs: [5, 49, 65, 68]
spec: 'docs/superpowers/specs/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-design.md'
plan: 'docs/superpowers/plans/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding.md'
results: 'docs/results/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'fix/permission-floor-and-wrapper-parity-across-modes-and-binding'
pr: 'https://github.com/ethanhinson/fuse/pull/116'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-design.md](../../superpowers/specs/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-design.md) |
| Spec (merged) | [2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-design.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/specs/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-design.md) |
| Plan | [2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding.md](../../superpowers/plans/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding.md) |
| Results | [2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-results.md](../../results/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-results.md) |
| ADRs | [ADR-0005](../../adrs/0005-per-segment-allow-rule-evaluation.md), [ADR-0049](../../adrs/0049-allowlist-admission-on-deterministic-allow-paths.md), [ADR-0065](../../adrs/0065-permission-patterns-match-by-consumer-generous-for-protectiv.md), [ADR-0068](../../adrs/0068-catastrophic-floor-is-a-rules-layer-decision-in-smart-mode-a.md) |
<!-- docket:artifacts:end -->

## Why

Three remaining mode and binding gaps in permission matching: a bare first-token smart allow such as `bash:rm` still approves `rm -rf ~` because the legacy first-token allow path has no destructive-command floor; protective rules naming a wrapper (`bash:timeout*`, `bash:nice *`) never fire in auto mode because auto matches the peeled segment; and the headless MCP server builds its gate without a workspace root, so the floor misses workspace ancestors such as `rm -rf ..`.

## What changes

- Apply the destructive-command floor to smart mode's legacy first-token allow path (a tightening; release note).
- Add the raw pre-peel first token as a protective subject in auto mode, so wrapper-name deny and ask rules match in both modes.
- Bind the workspace root on the headless MCP server gate.
- Tests for each.

## Out of scope

- Changing allow glob semantics.
- New permission modes or default rule sets.

## Reconcile log

### 2026-10-31

Unchanged against origin/main tip; dependency 92 is merged.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 0b63daafa31fe21fb3b044148aa21cd1da842aed
ran_at:   2026-10-31T21:22:00Z
<!-- docket:build-evidence:end -->
