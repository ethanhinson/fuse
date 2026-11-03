---
id: 132
slug: 'pipeline-runs-are-event-sourced-and-resumable'
title: 'Pipeline runs are event-sourced and resumable'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: [127, 131]
stacked_on:
related: [115]
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

A `pipeline_run` is a long multi-step DAG whose whole state (done/skipped/released maps, retry counts, routing decisions) lives in closure locals of one goroutine. That is exactly the work a failover must survive, and today a move leaves a dangling tool call and nothing that can tell what finished.

## What changes

Pipeline lifecycle events on the loop stream, a `PipelineRunStore` folded from them, and a resume policy that either re-drives the remaining steps from their recorded inputs on the blackboard or fails the run with a synthetic result. Tests cover both.

## Out of scope

Running steps on other replicas (0137); changes to the pipeline YAML/schema; idempotency of step side effects (a re-driven step may re-run its tools).
