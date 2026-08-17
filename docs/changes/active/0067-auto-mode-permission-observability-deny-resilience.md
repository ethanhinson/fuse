---
id: 67
slug: auto-mode-permission-observability-deny-resilience
title: Auto-mode permission observability + deny-resilience — measure every gate decision, stop dying on denials
status: proposed
priority: high
type: feat
created: 2026-08-17
updated: 2026-08-17
depends_on: []
related: [40, 51, 68, 69, 70]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-17-auto-mode-permission-observability-deny-resilience-design.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-17-auto-mode-permission-observability-deny-resilience-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-17-auto-mode-permission-observability-deny-resilience-design.md) |
<!-- docket:artifacts:end -->

## Why

Stage A of the auto-mode overhaul (product bar settled 2026-08-17: Claude Code / Cursor parity — auto mode must not hard-block routine dev operations, and runs must not die because a tool call was denied). Real session data shows denial amplification is the dominant run-killer: models retry denied calls verbatim, tripping the loop detector (11 observed `loop.detector.trip` events) and the escalation valve (8 trips, and a tripped valve is permanent for the session). Meanwhile the gate emits no events at all — asks (human prompts) and headless `AlwaysApprove` resolutions are invisible, so prompt frequency, the core auto-mode UX metric, cannot be measured. This change lands the measurement substrate first so the later loosening stages (#0068–#0070) are tunable against data.

## What changes

- `permission.decision` event for every gate resolution (ctx-carried `DecisionSink` mirroring `WithUserMessages`; layer vocabulary; ask emitted before the human approval call and `layer=human` outcome after).
- Typed denials: `tools.Result.Denied` / `DenyLayer`, plus per-layer actionable deny hints appended to denial output.
- Deny→retry resilience: policy-denied repeats stop feeding `ErrLoopDetected`; after 2 identical denials a synthetic nudge message is injected (and emitted as `user.input` for resume-fold fidelity); only post-nudge repetition kills a headless run.
- Recoverable valve: interactive trip becomes ONE resetting approval prompt; headless trip returns structured `ErrAutoModePaused` instead of a denial cascade.

## Out of scope

- Any softening of the rules layer, classifier prompts, or web_fetch floor (stages #0068/#0069).
- Shell-parse widening (#0070).
- OTEL projector mapping for the new kind (0051 stack can consume it separately).

## Open questions

<!-- none — spec is settled from the approved plan -->

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
