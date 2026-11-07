---
id: 143
slug: 'headless-runs-finish-through-a-structured-report-and-a-plain'
title: 'Headless runs finish through a structured report, and a plain stop is never success'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-07'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [42, 124]
discovered_from: []
adrs: [23]
spec: 'docs/superpowers/specs/2026-11-07-headless-runs-finish-through-a-structured-report-and-a-plain-design.md'
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
| Spec | [2026-11-07-headless-runs-finish-through-a-structured-report-and-a-plain-design.md](../../superpowers/specs/2026-11-07-headless-runs-finish-through-a-structured-report-and-a-plain-design.md) |
| ADRs | [ADR-0023](../../adrs/0023-structured-delegation-return-result-tool.md) |
<!-- docket:artifacts:end -->

## Why

A headless `fuse` run ends on any reply with no tool call, and the process then exits 0. Exit 0 therefore does not mean the work is done. Real backlog runs on 2026-11-06 showed this repeatedly: GLM spent 3.5 hours on #137 and exited 0 having changed nothing on the branch, DeepSeek stopped on "Let me run the tests now:" and exited 0, and MiniMax exited 0 with its work uncommitted.

The opt-in finish check asks once and cannot tell finished from gave up. On one run it kept the model going for 14 more replies with no edits. A caller cannot retry or escalate a failed run automatically, because every plain stop looks the same.

The core loops of the DeepSeek Harness and DeerFlow have the same weakness; their reliability comes from a completion layer above the loop. This change gives fuse that layer.

## What changes

Behind an opt-in setting, a headless root run finishes by calling a `report_result` tool with a state of `done`, `blocked`, `failed` or `no_change_needed`, a summary, and the tests actually run.

- A plain stop is no longer success. It starts a bounded continuation round with a fresh prompt to re-inspect the workspace. When the rounds run out, the outcome is `unfinished`.
- Exit codes tell the outcomes apart: 0 for `done` and `no_change_needed`, 3 for `blocked`, 4 for `failed`, 5 for `unfinished`. A durable event records the outcome.
- Rounds stop early when they produce no new successful operation.
- The harness checks claims against ground truth: `done` with no file changes is rejected once, and listed tests that no `bash` call ran are recorded as unverified.
- The first `blocked` report is rejected once, so a model cannot use it to quit without re-checking the condition.

With the setting off, behaviour is unchanged.

## Out of scope

- Runs that never reach a stop, such as a model spending nearly all its tokens on reasoning. Those need time or token limits.
- A separate evaluator model judging completion.
- Re-running tests, or otherwise proving that a `done` claim is correct.
- Automatic retry or escalation by a caller. This change only makes it possible.
- Hosted and interactive loops, and spawned children.
- Turning the setting on by default. That is a later, separate decision made by a person.
