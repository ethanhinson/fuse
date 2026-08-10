---
id: 50
slug: client-sdk
title: Client SDK — thin-client library, same API local-or-remote
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-10
depends_on: [48, 49]
related: [45, 48, 49]
discovered_from: [45]
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

The end state is that the user's other apps are **thin network clients** over a hosted fuse
service. A client SDK gives those apps a small library to import that exposes the **same API
whether the runtime is local or remote** — the same surface as the in-process `Runtime`, backed
by the networked binding (change 48) and its auth (change 49) when remote. This is what makes
"any AI app can target fuse" concrete: import the SDK, get a loop, local or hosted, identically.
Needs brainstorming: the SDK surface, the local-vs-remote transport switch, and language scope.

## What changes

To be designed during grooming. At a sketch: a thin-client library mirroring the `Runtime` API
that talks to the networked binding (change 48) with auth (change 49) when remote, or to an
in-process Runtime when local — one API surface, transport-transparent.

## Out of scope

To be defined during grooming. The service side (transport, auth) is changes 48 and 49.

## Open questions

- Target language(s) for the first SDK.
- The local-vs-remote switch — one API, two backends — and how transparent it should be.
- How much of the event-stream / replay-cursor semantics the SDK surfaces vs. hides.
