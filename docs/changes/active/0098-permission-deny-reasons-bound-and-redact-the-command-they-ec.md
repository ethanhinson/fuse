---
id: 98
slug: 'permission-deny-reasons-bound-and-redact-the-command-they-ec'
title: 'Permission deny reasons bound and redact the command they echo'
status: 'implemented'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [94, 96]
discovered_from: [96]
adrs: [71]
spec: 'docs/superpowers/specs/2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec-design.md'
plan: 'docs/superpowers/plans/2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec.md'
results: 'docs/results/2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'fix/permission-deny-reasons-bound-and-redact-the-command-they-ec'
pr: 'https://github.com/ethanhinson/fuse/pull/120'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec-design.md](../../superpowers/specs/2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec-design.md) |
| Plan | [2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec.md](../../superpowers/plans/2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec.md) |
| Results | [2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec-results.md](../../results/2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec-results.md) |
| ADRs | [ADR-0071](../../adrs/0071-commands-the-parser-refuses-are-never-answered-by-a-policy-s.md) |
<!-- docket:artifacts:end -->

## Why

Every rules, floor, heuristic and unparseable deny reason echoes the full shell command with no length cap and no redaction, while the structured command field on the decision is already bounded and has URL userinfo redacted. A long refused command puts a very large reason into a durable event and tool result on every attempt, and credentials embedded in a URL are repeated in prose. The rules-layer deny hint also calls every such deny catastrophic, which is wrong for explicit-ask, smart-mode and unparseable denies.

## What changes

- One helper bounds and redacts the command text used in every deny reason, matching the structured command preview.
- All deny reasons that echo a command use it, so they stay consistent.
- The rules-layer deny hint says the call was blocked by the permission rules rather than calling it catastrophic.
- Exact-string tests updated together.

## Out of scope

- Changing tool.call event payloads.
- Provenance semantics.

## Reconcile log

### 2026-10-31

Unchanged against main eb16b8a; spec holds.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: c13a9f5d24391b0afdf636909d97df7c17ac1725
ran_at:   2026-10-31T23:40:27Z
<!-- docket:build-evidence:end -->
