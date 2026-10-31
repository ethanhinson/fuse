---
id: 97
slug: 'approve-all-carries-across-the-cli-model-relay-and-policy-de'
title: 'Approve-all carries across the CLI-model relay, and policy denials say who denied'
status: 'proposed'
priority: 'low'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [94, 96]
discovered_from: [94]
adrs: [70]
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
| ADRs | [ADR-0070](../../adrs/0070-approval-provenance-is-per-gate-explicit-ask-rules-supersede.md) |
<!-- docket:artifacts:end -->

## Why

With a CLI-routed model, tools run in an MCP server subprocess that evaluates its own permission mode and now receives the parent's approval provenance. Under `--approve-all` the parent runs with permissions off, but the subprocess sees a policy approver and denies explicit ask matches, contradicting the flag's promise to approve every tool call. Separately, a deny decided by a policy stand-in still uses the reason text "tool call denied by user", which is misleading in an audit trail even though the structured decided_by field is right.

## What changes

- The approve-all posture is carried to the CLI-model subprocess, which then runs with permissions off, matching the parent.
- Deny reasons say "denied by approver" (or name policy) when the decider is not a human; decided_by stays the authority.
- Tests for both.

## Out of scope

- Changes to provenance semantics.
- New approval protocols.
