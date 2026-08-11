---
id: 56
slug: sdk-viability-hardening-wander
title: SDK viability hardening — dogfood @fuse/sdk by building Wander, fix what blocks a real web app
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [50]
related: [49, 54, 55]
discovered_from: [50]
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

Change #50 shipped two client SDKs (Go `sdk/fuse` + TS `@fuse/sdk`, over the #55
`fuse.loop.v1` Connect wire). Nothing has yet driven that SDK from a real application.
**Wander** is that forcing function: a vacation-rental-concierge **demo app in plain
HTML/CSS/JS** that shows off using fuse as an agent loop inside a web application
(startLoop → send → observe → transparent reconnect against a hosted loop). Wander itself
is the demo we make viable — it is NOT the deliverable. **The deliverable is one PR of
bugfixes + Q/A + the small, truly-blocking features that any real browser app needs before
it can drive a hosted fuse loop at all.** Building Wander is how we discover them; the
SDK/runtime fixes are the change.

## What changes

Proposal altitude — the design pass must decompose "make any web app viable" down to a
single PR's worth of scope. Seeds (from change #50's results file
`docs/results/2026-08-11-client-sdk-results.md`, linked from the archived change
`archive/2026-08-11-0050-client-sdk.md`):

- **Browser-reach proof, deferred at BOTH #55 and #50 merge gates:** drive the TS SDK in a
  REAL browser — startLoop → send → observe → reconnect over `@connectrpc/connect-web`, kill
  the network mid-stream, assert transparent reconnect resumes with no-loss/no-dup. #50 only
  proved this from a node test over the identical wire. Wander is where it gets exercised for
  real; a headless-browser CI lane may be part of this change.
- **The small must-have web-app features** the SDK surface is missing for a browser app to be
  viable at all — surface them by building Wander (ergonomic session/connection lifecycle,
  error/reconnect UX hooks, whatever the real app hits).
- **Bugs / rough edges** in the SDK's startLoop→send→observe→reconnect surface that only
  appear under a real app.

## Out of scope

- **Durable/resumable sessions (refresh-to-restore, cross-device resume) = change #54.** This
  change does NOT re-litigate transcript durability or the REST resume surface; it consumes
  whatever #54 lands. State the boundary clearly so the two don't overlap. If a Wander
  must-have REQUIRES durable sessions, note it as a dependency for the brainstorm to resolve
  rather than absorbing #54's scope.
- **Python / mobile-native SDKs** — later, separate changes (#50 follow-ups).
- **A batteries-included Go runtime-from-config builder** — explicitly out of scope per
  ADR-0035.

## Note

Keep it to ~one PR. Flag for the brainstorm: (1) settle the #54 boundary, (2) decide whether
the real-browser CI lane lands here or stays a manual gate, (3) decompose "viable web app" to
a concrete must-have list driven by actually building Wander. Filed 2026-08-11 as a #50
follow-up (auto_capture disabled this repo, so captured by hand).

## Open questions

- The #54 durable-sessions boundary: which Wander must-haves (if any) actually require durable
  sessions, and are they a `depends_on` rather than in-scope work here?
- Does the real-browser reconnect proof land as a headless-browser CI lane in this change, or
  stay a manual gate?
- What is the concrete must-have web-app feature list once Wander is actually built?

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
