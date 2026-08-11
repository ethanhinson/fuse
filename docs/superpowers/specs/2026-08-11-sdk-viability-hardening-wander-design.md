<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0056 — SDK viability hardening — dogfood @fuse/sdk by building Wander, fix what blocks a real web app](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0056-sdk-viability-hardening-wander.md)**
<!-- docket:backlink:end -->

# Design — SDK viability hardening via Wander (change 0056)

## Problem

Change #50 shipped two client SDKs — Go `sdk/fuse` and TS `@fuse/sdk` — over the #55
`fuse.loop.v1` Connect wire. They pass their own tests (including a node-level no-loss/no-dup
property test), but **no real application has ever driven them**. The known unknowns are exactly
the things unit tests over a controlled wire cannot surface:

- Whether the SDK's public surface is *ergonomic enough* for an app author to wire
  `startLoop → send → observe → reconnect` without reaching past the SDK into the generated
  Connect stubs.
- Whether transparent reconnect actually works **in a browser** — the reconnect/no-loss-no-dup
  property was proven only from a node test at #50, and a real-browser proof was explicitly
  deferred at BOTH the #55 and #50 merge gates.
- Which small features a real browser app *cannot function without* (error/lifecycle hooks,
  connection-state signals, a clean teardown) that the SDK does not yet expose.

We close these by **building Wander** — a vacation-rental-concierge demo in plain HTML/CSS/JS —
as the forcing function. Wander is the acceptance vehicle, not the deliverable: the change's value
is the SDK bugfixes + must-have features + the permanent real-browser test lane that building it
surfaces. The demo app ships alongside as a committed example so the dogfood loop is repeatable.

## Decision — one PR: Wander + the fixes it surfaces

Per grooming (2026-08-11): **build Wander and fix what it surfaces in a single PR.** Wander ships
as a committed example app (`examples/wander/`), and its concrete features are the spec's
acceptance criteria; the SDK changes land as they are discovered while building against them.

### Wander — the demo app (the acceptance vehicle)

A single-page vacation-rental concierge in plain HTML/CSS/JS (no framework), consuming `@fuse/sdk`
over `@connectrpc/connect-web` against a hosted fuse loop:

- **Concierge chat.** A chat panel: the user asks about rentals ("beachfront, sleeps 6, under
  $300/night"), the agent loop responds. Drives `startLoop` on first message, `send` per user
  turn, `observe` for the streamed reply — the persistent-loop mechanics (#53) so the concierge
  holds context across turns.
- **Streaming replies.** Render the loop's event stream incrementally (assistant text as it
  arrives; tool activity surfaced as concierge "looking things up…" affordances), keyed on the
  `Observe(fromSeq)` cursor. Completion shown from the explicit `loop.parked`/`IsCompletion`
  event, never inferred from stream shape (learning:
  `persistent-loop-needs-explicit-completion-event`).
- **Reconnect UX.** A visible connection-state indicator (live / reconnecting / offline). Killing
  the network mid-reply must transparently resume with no lost or duplicated messages — the
  property proven at the node level in #50, now demonstrated in a real browser and asserted in CI.

Wander is deliberately **stateless across page loads**: a refresh starts a fresh session. That is
acceptable for a demo and is what holds the #54 boundary (below).

### SDK fixes & must-have features (discovered by building Wander)

The concrete list is finalized against the real app during build, but the seeded, near-certain
must-haves are:

- **Connection-state / lifecycle hooks.** The SDK must expose connection state
  (connecting / live / reconnecting / closed) and lifecycle callbacks so an app can render a
  connection indicator and react to reconnect without reaching into the transport. #50's TS SDK
  reconnects internally (exponential backoff, added in review) but does not *surface* that state.
- **Error surfacing.** Distinguish a transient/reconnecting error from a terminal one (auth
  rejected, loop finished) at the SDK boundary, so the app shows the right affordance instead of a
  raw Connect error. Reconnect classification must treat an abnormal mid-stream drop as resumable,
  not as a fatal error (learning: `websocket-read-errors-are-not-closeerror`, ported to the
  Connect-stream shape).
- **Clean teardown.** An explicit, idempotent way to stop observing and release the stream on
  page unload / component teardown — a real app must not leak a stream per navigation.
- **Whatever else Wander hits.** Genuine rough edges found while wiring the app are in scope; each
  is recorded in the results file with the Wander interaction that surfaced it.

### Real-browser reconnect proof — a permanent headless CI lane

Per grooming: add an automated **headless-browser (Playwright / headless Chromium)** lane that
drives `@fuse/sdk` over `@connectrpc/connect-web` against a real `connect-go` server, kills the
network mid-stream, and asserts transparent resume with **no loss / no dup** — the exact check
deferred at #55 and #50, now permanently enforced in CI rather than left as a manual gate.

- The lane must be **loud**: it hard-fails (does not silently skip) when its browser toolchain is
  absent, so a green suite can never hide an unexercised browser path (learning:
  `smoke-over-fake-backend-proves-wire-not-system`).
- The dedup-at-watermark property is the specific correctness target: subscribe-live-then-replay
  must not double-deliver across the handoff (learning:
  `replay-live-handoff-dedup-at-watermark`).
- The server side reuses #50's real-`connect-go`-server test harness with the scripted
  `LLM_GATEWAY_URL` double — **never Claude/Anthropic** (project policy; feedback:
  live verify traffic uses the cheap gateway, never Claude).

## Scope boundary — #54 (durable, resumable sessions)

This change does **not** implement or re-litigate durable/resumable sessions
(refresh-to-restore, cross-device resume, transcript rehydration, a REST resume surface). That is
change #54. Wander is designed **stateless across page loads** precisely so it needs nothing #54
owns: it demonstrates a live, reconnecting session, not a session that survives a tab close.

`depends_on` stays `[50]` only — **not** `[54]`. If, while building Wander, a demo-critical feature
turns out to genuinely require durable sessions, the correct move is to add `depends_on: [54]` and
descope that feature here (recorded in the reconcile log), NOT to absorb #54's persistence work
into this change.

## Out of scope

- **Durable / resumable sessions** — change #54 (boundary above).
- **Python / mobile-native SDKs** — later, separate changes (#50 follow-ups).
- **A batteries-included Go runtime-from-config builder** — out of scope per ADR-0035; Wander is a
  browser app over the TS remote SDK, so it does not need the Go local backend at all.
- **A production deployment / hosting story for Wander** — it is an example app run against a
  local or dev-hosted loop; productionizing the demo is not this change.

## Acceptance criteria

1. `examples/wander/` exists: a plain HTML/CSS/JS concierge app that, against a running hosted
   loop, holds a multi-turn concierge conversation with streaming replies and a visible
   connection-state indicator, driven entirely through `@fuse/sdk`'s public API (no direct
   generated-stub access from app code).
2. The SDK exposes connection-state/lifecycle hooks, distinguishes transient from terminal errors,
   and offers idempotent teardown — each with a test, each consumed by Wander.
3. A **headless-browser CI lane** drives the SDK over `connect-web`, kills the network mid-stream,
   and asserts transparent resume with no-loss/no-dup; it hard-fails loudly when its toolchain is
   absent.
4. Every SDK bug/rough-edge fixed is recorded in the results file with the Wander interaction that
   surfaced it.
5. Go + TS suites green; the new browser lane green; `LLM_GATEWAY_URL` double used for any
   live-loop traffic (never Claude/Anthropic).

## Open questions (for build-time reconcile)

- The exact must-have feature list is finalized against the real app during build (that is the
  point of dogfooding); the seeds above are the near-certain core.
- Playwright vs. a lighter headless-Chromium harness is a build-time toolchain choice — the
  requirement is "a real browser, network-killed mid-stream, in CI, loud on absence," not a
  specific tool.
