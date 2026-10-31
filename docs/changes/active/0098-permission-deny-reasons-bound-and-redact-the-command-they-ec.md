---
id: 98
slug: 'permission-deny-reasons-bound-and-redact-the-command-they-ec'
title: 'Permission deny reasons bound and redact the command they echo'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [96, 97]
discovered_from: [96]
adrs: [71]
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
| Artifact | Link |
|---|---|
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
