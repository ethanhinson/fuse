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
spec: docs/superpowers/specs/2026-08-11-sdk-viability-hardening-wander-design.md
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
| Spec | [2026-08-11-sdk-viability-hardening-wander-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-sdk-viability-hardening-wander-design.md) |
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

**One PR: build Wander, fix what it surfaces.** Wander — a vacation-rental-concierge demo in
plain HTML/CSS/JS, shipped as a committed example (`examples/wander/`) over `@fuse/sdk` — is the
acceptance vehicle, not the deliverable. Its concrete features are the spec's acceptance criteria;
the SDK bugfixes and must-have features land as building against a real app surfaces them. Design
detail in the linked spec. Three parts:

- **Wander (the forcing function).** A concierge chat: multi-turn conversation over one persistent
  loop (#53), streaming replies keyed on `Observe(fromSeq)`, completion from the explicit
  `loop.parked`/`IsCompletion` event, and a visible connection-state indicator. Stateless across
  page loads (a refresh starts a fresh session) — deliberately, to hold the #54 boundary.
- **SDK must-have features & bugfixes.** Connection-state / lifecycle hooks; transient-vs-terminal
  error surfacing at the SDK boundary (abnormal mid-stream drop = resumable, not fatal); idempotent
  teardown to release a stream on unload; plus whatever genuine rough edges Wander hits — each
  recorded in the results file with the interaction that surfaced it.
- **Real-browser reconnect proof as a permanent CI lane.** A headless-browser (Playwright /
  headless Chromium) lane driving `@fuse/sdk` over `@connectrpc/connect-web` against a real
  `connect-go` server, killing the network mid-stream and asserting transparent resume with
  no-loss/no-dup — the check deferred at #55 and #50, now enforced in CI. Loud on toolchain
  absence; scripted `LLM_GATEWAY_URL` double, never Claude/Anthropic.

## Out of scope

- **Durable/resumable sessions (refresh-to-restore, cross-device resume) = change #54.** Wander is
  designed stateless-across-page-loads precisely so it needs nothing #54 owns. `depends_on` stays
  `[50]`, not `[54]`. If a demo-critical feature turns out to genuinely require durable sessions,
  the move is to add `depends_on: [54]` and descope that feature here (recorded in the reconcile
  log) — NOT to absorb #54's persistence work.
- **Python / mobile-native SDKs** — later, separate changes (#50 follow-ups).
- **A batteries-included Go runtime-from-config builder** — out of scope per ADR-0035; Wander is a
  browser app over the TS remote SDK and does not touch the Go local backend.
- **A production deployment / hosting story for Wander** — it is an example app run against a local
  or dev-hosted loop.

## Open questions

- The exact must-have feature list is finalized against the real app at build time (that is the
  point of dogfooding); the spec's seeds are the near-certain core.
- Playwright vs. a lighter headless-Chromium harness is a build-time toolchain choice — the
  requirement is "a real browser, network-killed mid-stream, in CI, loud on absence."

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
