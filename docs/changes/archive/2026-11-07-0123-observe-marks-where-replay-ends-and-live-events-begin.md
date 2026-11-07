---
id: 123
slug: 'observe-marks-where-replay-ends-and-live-events-begin'
title: 'Observe marks where replay ends and live events begin'
status: 'killed'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [117]
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

A caller that opens `Observe` from sequence 0 receives a replay of history followed by live events, and nothing marks the boundary between the two. A caller that must tell them apart, for example to decide whether a loop is still active, has to compare event timestamps from the runtime host's clock with its own clock, or wait for the first keepalive. Clock skew makes both unreliable.

## What changes

- `Observe` emits a marker once replay has caught up to the stream head (for example a `stream.live` control frame carrying the head sequence number). Alternatively, the response reports the latest sequence number at subscription time.
- Document it, add it to the TypeScript SDK, and test it with a replay followed by live events.

## Out of scope

Changing event retention or the replay order.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared server. Observe replay markers only serve the network server, which is being deleted.
