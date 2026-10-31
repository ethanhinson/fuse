---
id: 92
slug: 'smart-mode-honors-always-prompt-with-no-human-approver-bound'
title: 'Smart mode honors always_prompt with no human approver bound'
status: 'in-progress'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: [87, 88]
stacked_on:
related: [87, 88]
discovered_from: [87]
adrs: [64]
spec: 'docs/superpowers/specs/2026-10-31-smart-mode-honors-always-prompt-with-no-human-approver-bound-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch: 'fix/smart-mode-honors-always-prompt-with-no-human-approver-bound'
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-smart-mode-honors-always-prompt-with-no-human-approver-bound-design.md](../../superpowers/specs/2026-10-31-smart-mode-honors-always-prompt-with-no-human-approver-bound-design.md) |
| ADRs | [ADR-0064](../../adrs/0064-explicit-ask-rules-are-their-own-verdict-prompt-a-human-deny.md) |
<!-- docket:artifacts:end -->

## Why

Explicit ask rules now deny in auto mode when no human approver is bound, but in smart mode (the loop server's default when no spec sets auto) an `always_prompt` match under a policy stand-in is still routed to the always-approve approver and allowed. The same implicit allow the explicit-ask decision removes survives one mode over, so operators get different safety from the same rule depending on mode.

## What changes

- In smart mode, an `always_prompt` match under policy provenance denies at the rules layer with the matched pattern in the reason, as in auto mode.
- With a human approver bound, smart mode prompts as before.
- Tests for both provenances in smart mode, plus an end-to-end hosted-loop test.

## Out of scope

- Glob matching semantics.
- A client-visible approval protocol.
- Other smart-mode behavior.
