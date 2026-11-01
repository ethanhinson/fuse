---
id: 110
slug: 'mark-loopsecret-value-debug-redact-once-protobuf-go-honours'
title: 'Mark LoopSecret.value debug_redact once protobuf-go honours it'
status: 'proposed'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: [102]
discovered_from: [102]
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

protobuf-go v1.36.12 prints LoopSecret values through String, prototext and protojson; the debug_redact field option would redact them once the library supports it.

## What changes

- Track the upstream protobuf-go release with redaction support.
- Add debug_redact to LoopSecret.value and a test that String, prototext and protojson redact it.

## Out of scope

- Custom redaction in fuse.
