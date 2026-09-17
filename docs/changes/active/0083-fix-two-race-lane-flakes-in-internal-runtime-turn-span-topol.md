---
id: 83
slug: fix-two-race-lane-flakes-in-internal-runtime-turn-span-topol
title: 'Fix two race-lane flakes in internal/runtime — turn-span topology and Send-enqueue tests'
status: proposed
priority: medium
type: fix
created: 2026-09-17
updated: 2026-09-17
depends_on: []
related: []
discovered_from: [75]
adrs: []
spec:
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
<!-- docket:artifacts:end -->

## Why

Two `internal/runtime` tests are timing-flaky under the race detector and have failed the `observability-acceptance` CI job on a branch that does not touch `internal/runtime` or `internal/agent` at all (PR #91, run 35178520316; its twin run on the same sha passed):

- `TestTurnScopedTraceTopologyThroughRealOTELExporter` — `exported fuse.loop.turn spans after two later turns = 1, want 2`. Reproduced on `main` at dea3ce6: 4 failures in 100 runs (`go test -race -count=50 -cpu 1,2`), and 2/100 at b966b5e. The test does `waitForKind(KindLoopParked)` then `flush(provider)` then counts exported spans; the second turn's span has evidently not always ended by the time the park event is observed, so the flush exports one span.
- `TestSendEnqueuesForRoot` — `root queue = [], want one 'more work' message`. Not reproduced locally in 100 runs; failed once in CI. It asserts the enqueue synchronously right after `Send` while the run goroutine is live.

A flake in a job that gates every PR erodes the gate: people learn to rerun instead of read.

## What changes

- Make the turn-span assertion wait for the span to END, not for the park event (or make the park event the last thing emitted after the span closes — whichever the runtime's ordering contract actually is; record it).
- Make `TestSendEnqueuesForRoot` observe the queue through the same synchronisation the injector uses, or poll with a bounded deadline.
- Run both under `-race -count=200` before and after as the evidence.

## Out of scope

- Any change to production runtime ordering beyond what the ordering contract already promises.

## Open questions

- Is the park event legitimately allowed to precede the turn span's end? If yes the test is wrong; if no the runtime is.
