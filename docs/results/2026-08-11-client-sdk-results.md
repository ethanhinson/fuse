<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0050 — Client SDK — Runtime-parity Go + TS/JS libraries, same API local-or-remote](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0050-client-sdk.md)**
<!-- docket:backlink:end -->

# Results — Client SDK (change 0050)

## Summary

Shipped **two client SDKs** presenting the fuse Runtime-parity loop-control surface
(`StartLoop` / `Send` / `Observe`, keyed on `loopID`) over change 0055's `fuse.loop.v1`
Connect/protobuf wire:

- **Go SDK** (`sdk/fuse`, importable at `github.com/ethanhinson/fuse/sdk/fuse`) — a **local**
  backend over a pre-built in-process `runtime.Runtime` (ADR-0035) **and** a **remote** backend
  dialing the generated `connect-go` client, one constructor switch. "Same API local-or-remote."
- **TS/JS SDK** (`sdk/ts`, `@fuse/sdk`, a new monorepo npm workspace) — **remote-only** over the
  generated `connect-es` stubs, so the browser (Wander, a later change) can drive a hosted loop.

Both surface the persistent-loop mechanics first-class: the event stream, the replay cursor
(`Observe(fromSeq)`), gap markers, and the explicit completion event (`loop.parked` →
`IsCompletion`) — never inferred from stream shape.

**#50 consumes #55's already-shipped wire and stubs and #49's already-shipped auth seam** — it does
NOT define the wire, generate stubs, or implement an auth mechanism. See the change's
`## Reconcile log` for the transport re-map (spec written pre-#55) and the scope shrinkage.

| Component | Backend(s) | Key tests |
|---|---|---|
| Go `sdk/fuse` | local (seam) + remote (Connect) | 13 tests: round-trip, no-loss/no-dup (local & remote), auth reject/spoof, completion, **real-loop E2E** |
| TS `@fuse/sdk` | remote-only (connect-es) | node test over a real `connect-go` server: no-loss/no-dup **from TS** across the subscribe→replay gap |

## Verification

- Go: `go build ./...` green; `go test ./sdk/...` — 13/13 pass (incl. `TestRealLoopAcceptance`, a
  real engine over the wire via a `fuse loop-serve-net` subprocess with a **scripted
  `LLM_GATEWAY_URL`** — never Claude/Anthropic, project policy); `go vet` and `-race` clean.
- TS: `cd sdk/ts && npm install && npm test` — 1 pass, **0 skipped**, exit 0; `tsc --noEmit` clean;
  `make sdk-ts-test` exit 0.
- The TS lane is **loud** (`make sdk-ts-test` hard-fails if `node` is absent) — no silent `t.Skip`
  can hide an unexercised cross-language path (`smoke-over-fake-backend-proves-wire-not-system`).

## Verify (human) — manual checks for the merge gate

- [ ] **Browser reach (deferred manual proof).** Drive the TS SDK in a **real browser** (Playwright
      or manual): `startLoop → send → observe → reconnect` over `@connectrpc/connect-web`, killing
      the network mid-stream and asserting the transparent reconnect resumes with **no loss / no
      dup**. This is the manual check deferred at #55's merge gate; #50's node test proves the wire
      and the no-loss/no-dup property from TS over the identical Connect wire, but a headless-browser
      CI lane is heavier than this change should add. Recorded here rather than left implicit.

## Findings / decisions

- **ADR-0035 — Go SDK local backend takes a pre-built `runtime.Runtime`** (not config-to-build).
  Building a runtime requires the `cmd/fuse` composition root, which is `package main` and
  un-importable; so `NewLocal(rt runtime.Runtime, …)` takes the runtime the embedding app already
  built. Keeps `sdk/fuse` a small importable client. See `docs/adrs/0035-*`.
- **Reconcile re-map (not a halt).** The spec was authored before #55 landed and described the SDKs
  as *generating* clients from an undesigned IDL. #55 shipped the wire + Go/TS stubs; all design
  decisions held, so the reconcile re-mapped the transport mechanics (WS → Connect stream; separate
  HTTP replay → `Observe(fromSeq)` server-stream; ADR-0032 → 0033) rather than halting
  (`reconcile-transport-swapped-under-spec-remap-not-halt`).
- **Real-loop acceptance closes #55's fake-backend gap.** #55's `connect-es` smoke ran against a
  `fakeRuntime` and `t.Skip`ped silently; #50 adds a real-engine E2E and a loud CI lane.

## Plan deviations

- **Skill-role fallbacks (missing-skill rule).** `superpowers:writing-plans`,
  `subagent-driven-development`, `requesting-code-review`, and `finishing-a-development-branch` are
  not invocable on this machine, so each **degraded to `auto`** per the convention's missing-skill
  rule: the plan was authored directly; the build ran as docket-native `docket-build-standard`
  subagents (TDD, per-task commits); the review was a whole-branch audit; the PR is opened via the
  auto finish-fallback. Noted in the PR body.
- **Review fixes applied before PR.** The whole-branch review flagged a hot-spin TS reconnect loop
  (no backoff) and an implicit stream cleanup — both fixed (exponential backoff capped at 30s, reset
  on a healthy stream; explicit per-pass stream teardown). Its two "blockers" were the Go README and
  this results file (plan Task 8), both now present.

## Follow-ups

- A batteries-included Go convenience builder ("give me a runtime from config") is explicitly out of
  scope (ADR-0035 Consequences); if wanted, a separate heavier package outside `sdk/fuse`.
- A Python / mobile-native SDK, and the Wander app that consumes this TS SDK, are later changes.
  (`auto_capture` disabled this repo — none minted; captured here.)
