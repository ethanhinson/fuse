---
id: 56
slug: sdk-viability-hardening-wander
title: SDK viability hardening — dogfood @fuse/sdk by building Wander, fix what blocks a real web app
status: in-progress
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [50]
related: [49, 54, 55]
discovered_from: [50]
adrs: []
spec: docs/superpowers/specs/2026-08-11-sdk-viability-hardening-wander-design.md
plan: docs/superpowers/plans/2026-08-11-sdk-viability-hardening-wander-plan.md
results:
trivial: false
auto_groomable:
branch: feat/sdk-viability-hardening-wander
claimed_at: 2026-08-11T23:41:00Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-sdk-viability-hardening-wander-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-sdk-viability-hardening-wander-design.md) |
| Plan | [2026-08-11-sdk-viability-hardening-wander-plan.md](https://github.com/ethanhinson/fuse/blob/feat/sdk-viability-hardening-wander/docs/superpowers/plans/2026-08-11-sdk-viability-hardening-wander-plan.md) |
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

### 2026-08-11 — reconciled against origin/main (b7cebf4); spec holds, one naming clarification, no scope change

Reconciled #56 against current `origin/main` after `#50`/`#55` reached `done` (both archived
2026-08-11) and `#59` (mcp-full-binding-wiring) merged (`origin/main` tip `b7cebf4`). Verified all
spec claims against `origin/main` via `git show`/`git ls-tree` (learning
`reconcile-verify-claims-against-origin-not-working-tree`). **The spec's design decisions all hold
on current code — no re-map, no invalidation, no obsolescence. `depends_on: [50]` stays correct
(#50 is `done`; #54 is still `proposed` and deliberately NOT a dependency — Wander stays
stateless-across-page-loads).**

Current-code anchors confirmed (the build targets these, not the spec's prose):

- **TS SDK `@fuse/sdk` at `sdk/ts/`** (root npm workspace: `package.json` workspaces = `proto`,
  `sdk/ts`). Public surface today: `createClient({baseUrl, credentials:{token,tenant}, transport?})`
  → `startLoop / send / observe / isCompletion / KIND_LOOP_PARKED`. `observe(loopId, fromSeq?)` is an
  async iterable that reconnects transparently with exponential backoff (cap 30s), tracks `seq`,
  dedups at the watermark, surfaces `gap`. The three seeded must-haves are genuinely absent and
  confirmed as real gaps: **(1)** no connection-state surface (connecting/live/reconnecting/closed) —
  reconnect is purely internal; **(2)** no transient-vs-terminal error classification — the reconnect
  loop's `catch {}` swallows *every* error, so a terminal condition (auth rejected, loop finished)
  hot-loops forever instead of surfacing; **(3)** no explicit/idempotent teardown beyond breaking the
  `for await` (no `close()`/AbortSignal for page-unload). These are exactly the spec's acceptance-2
  features and each is Wander-consumed.
- **Wire `fuse.loop.v1`** (`proto/fuse/loop/v1/loop.proto`, gen TS at `proto/gen/ts/...`,
  gen Go at `internal/loopwire/v1` + `loopv1connect`): `StartLoop`/`Send` unary, `Observe(from_seq)`
  server-stream (history-then-live), completion is the explicit `loop.parked` event kind. No wire work
  is in scope — the SDK builds on this.
- **Existing `examples/concierge-demo/`** is a *prior*, WS-era concierge over binding #3's raw
  `loop.*` JSON-RPC (change #48) with a Node relay — **it does NOT use `@fuse/sdk`.** The spec's
  `examples/wander/` is genuinely additive: the SDK-driven (`@connectrpc/connect-web` over the #55
  Connect wire) concierge. Clarification only, no scope change; the new app is not a rewrite of the
  old demo and the old demo is left untouched.
- **CI (`.github/workflows/integration.yml`)** has `unit-race` (Go `-race`) + `mcp-integration`
  (Docker + **playwright-go** chromium already installed). There is **no TS/node lane and no
  browser lane driving `@fuse/sdk`** — the acceptance-3 headless-browser reconnect lane is net-new,
  with `playwright-go` as an in-repo toolchain precedent.
- **Gateway-double harness pattern** established by `sdk/fuse/acceptance_test.go`: build `cmd/fuse`,
  exec `fuse loop-serve-net --addr 127.0.0.1:<port>` with a scripted in-test `LLM_GATEWAY_URL`
  (never Claude/Anthropic — project policy) and the built-in dev token `fuse-dev-token` → tenant
  `_default`. The browser lane reuses this real-engine harness (and `sdk/ts/test/server/main.go`'s
  `URL <url>`-printing fakeRuntime pattern for the pure-wire/dedup slice). Relevant learnings for the
  build: `scripted-gateway-double-double-escapes-tool-args` (pass PLAIN JSON tool args),
  `smoke-over-fake-backend-proves-wire-not-system` (keep the lane loud on toolchain absence),
  `replay-live-handoff-dedup-at-watermark`, `persistent-loop-needs-explicit-completion-event`,
  `websocket-read-errors-are-not-closeerror` (ported to the Connect-stream shape for error
  classification), `httptest-defer-close-before-tcleanup-deadlock`.

No auto-capture: `AUTO_CAPTURE_ENABLED=false` this repo — any adjacent follow-ups surfaced during
build are reported in the run report, not minted.
