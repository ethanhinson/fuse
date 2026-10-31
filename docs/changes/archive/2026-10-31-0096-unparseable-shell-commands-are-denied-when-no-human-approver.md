---
id: 96
slug: 'unparseable-shell-commands-are-denied-when-no-human-approver'
title: 'Unparseable shell commands are denied when no human approver is bound'
status: 'done'
priority: 'high'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: [94]
stacked_on:
related: [87, 92, 93, 94]
discovered_from: [93]
adrs: [66, 68, 71]
spec: 'docs/superpowers/specs/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-design.md'
plan: 'docs/superpowers/plans/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver.md'
results: 'docs/results/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'fix/unparseable-shell-commands-are-denied-when-no-human-approver'
pr: 'https://github.com/ethanhinson/fuse/pull/119'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-design.md](../../superpowers/specs/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-design.md) |
| Spec (merged) | [2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-design.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/specs/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-design.md) |
| Plan | [2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver.md](../../superpowers/plans/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver.md) |
| Results | [2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-results.md](../../results/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-results.md) |
| ADRs | [ADR-0066](../../adrs/0066-explicit-ask-rules-are-their-own-verdict-in-auto-and-smart-m.md), [ADR-0068](../../adrs/0068-catastrophic-floor-is-a-rules-layer-decision-in-smart-mode-a.md), [ADR-0071](../../adrs/0071-commands-the-parser-refuses-are-never-answered-by-a-policy-s.md) |
<!-- docket:artifacts:end -->

## Why

Shell commands the parser refuses (for example `sudo rm -rf /`, `/bin/rm -rf /`, or a non-inert `env` prefix) cannot be matched against deny rules, the destructive-command floor or explicit ask rules. In auto mode they become an ask, and in smart mode no segment matches; under a policy stand-in (hosted loop servers, headless MCP server, piped-stdin one-shot) that ask is approved. So the most dangerous commands are the ones that slip past every protective layer when no person is present. Protective rules naming a wrapper also miss these forms in auto mode.

## What changes

- Under policy provenance, an unparseable shell command is a rules-layer deny with a clear reason, in auto and smart mode alike.
- With a human approver bound, it still prompts.
- Protective rules (deny, ask, always_prompt) are also matched against the raw command text for unparseable commands in auto mode, as smart mode already does.
- ADR extending the explicit-ask rule to unparseable commands.
- Tests for each form, including an end-to-end hosted-loop test.

## Out of scope

- Extending the shell parser to accept more forms.
- A client-visible approval protocol.

## Reconcile log

### 2026-10-31

Reconciled against main 1d30374 (0094 merged: permissions.Approver{Answer,DecidedBy}; the gate's provenance is g.approvalProvenance, set from approve.DecidedBy). gate.go resolveAuto/resolve structure and line ranges match the spec; scope unchanged. The ADR relates to ADR-0070 (which supersedes ADR-0066) and ADR-0068.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 5b858391c7db6e21cdc8129a54c401eab60b946d
ran_at:   2026-10-31T23:03:30Z
<!-- docket:build-evidence:end -->
