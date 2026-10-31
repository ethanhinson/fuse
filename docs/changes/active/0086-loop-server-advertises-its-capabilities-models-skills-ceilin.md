---
id: 86
slug: 'loop-server-advertises-its-capabilities-models-skills-ceilin'
title: 'Loop server advertises its capabilities - models, skills, ceilings'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [84, 85]
discovered_from: []
adrs: [60, 61]
spec: 'docs/superpowers/specs/2026-10-31-loop-server-advertises-its-capabilities-models-skills-ceilin-design.md'
plan: 'docs/superpowers/plans/2026-10-30-loop-server-advertises-its-capabilities-models-skills-ceilin.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/loop-server-advertises-its-capabilities-models-skills-ceilin'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-loop-server-advertises-its-capabilities-models-skills-ceilin-design.md](../../superpowers/specs/2026-10-31-loop-server-advertises-its-capabilities-models-skills-ceilin-design.md) |
| Plan | [2026-10-30-loop-server-advertises-its-capabilities-models-skills-ceilin.md](../../superpowers/plans/2026-10-30-loop-server-advertises-its-capabilities-models-skills-ceilin.md) |
| ADRs | [ADR-0060](../../adrs/0060-harnessspec-proto-is-the-portable-harness-definition-client.md), [ADR-0061](../../adrs/0061-a-loop-runs-a-client-harnessspec-layered-over-the-operator-c.md) |
<!-- docket:artifacts:end -->

## Why

A client that starts loops from a HarnessSpec has to guess what the server supports: which models it routes, which skills are installed, which tighten-only ceilings the operator set, which secret refs resolve. Every wrong guess is a StartLoop refusal. Models are free text because nothing lists them. A discovery RPC lets clients build model pickers and pre-flight a spec instead of finding out by being refused.

## What changes

- New authenticated, read-only `LoopService.GetCapabilities` RPC with the same auth as StartLoop.
- It returns advertised models: alias, gateway id, context window, max output, persona, default flag, and optional operator label, description and tags.
- It also returns installed skills, the effective ceiling for each tighten-only field (keyed by violation field path), the secret ref names that resolve for the caller's tenant, the MCP auth kinds a spec may use, and the server version and digest algorithm.
- Model config entries gain optional `label`, `description`, `tags` and `advertise` keys.
- The public Go SDK remote client gains `Capabilities(ctx)`.
- A golden test proves the advertised answer matches what StartLoop accepts and refuses.

## Out of scope

- Live gateway queries for model capabilities or pricing.
- A dry-run validate RPC.
- Changing tighten-only semantics for fields the operator leaves unset.
- The ignored StartLoop `model` field and the `static` MCP auth dial failure.
- Showing the new metadata in the TUI.

## Reconcile log

### 2026-10-31

Unchanged against main ae9c8d7; related #84 and #85 are merged and archived, so the spec layering and tighten-only options it builds on are present.

## Run halted

### 2026-10-31

Build role halted at plan Task 1: the docket-build-standard worker returned BLOCKED.

- Cause: the build-task contract requires every test command to run under `timeout --kill-after=10s 10m` (or `gtimeout`). Neither binary is on PATH on this machine (GNU coreutils not installed), so the worker could not run its focused tests.
- Remedy: install a timeout wrapper (for example `brew install coreutils`, which provides `gtimeout`), then resume this change by id through the run tracker (`run.start implement-next --resume 86`, then `change.resume-halted --acknowledge-quiescent`).
- State left: feature worktree /Users/ethanhinson/dev/fuse/.worktrees/loop-server-advertises-its-capabilities-models-skills-ceilin at HEAD 6aafbe1 (spec commit only), with uncommitted Task 1 RED tests: internal/config/models_metadata_test.go (new), internal/configcheck/configcheck_test.go, internal/harnessconfig/harnessconfig_test.go, internal/tui/models_editor_test.go. A resumed worker should keep them and continue at Task 1 Step 3.
- Plan attached; reconcile done (unchanged). No commits beyond the spec copy; no gate run; no evidence.
