<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0055 — Connect/protobuf transport — IDL-defined loop.* wire, successor to](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0055-grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4.md)**
<!-- docket:backlink:end -->

# Connect/protobuf transport (#55) — results
Change: #55 · Branch: feat/grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4 · PR: <set-at-open> · Plan: docs/superpowers/plans/2026-08-11-grpc-connect-transport-plan.md · ADRs: 33 (supersedes 32)

## Verify (human)

Automated coverage is green (`go test ./... -race`, 27 packages, 0 fail — including the concurrent
live+replay dedup test, the two-instance different-instance reconnect over the durable fsstore, the
idle-parked keepalive survival test, and a node connect-es TS smoke against a live connect-go server).
The one thing NOT run in CI:

- [ ] **Real-browser connect-es reconnect.** The connect-es server-streaming reconnect was proven from
      a node client using the REAL generated TS stub (`proto/smoke/client.ts`, driven by
      `internal/loopconnect/smoke_ts_test.go`), not from an actual browser. Confirm in a browser (the
      wire is identical: unary `StartLoop`/`Send` + server-streaming `Observe(from_seq)` over h2c, no
      proxy). This is the Wander testbed's core assertion and is #50's natural first exercise.
- [ ] **(optional) buf regen parity.** `cd proto && ./generate.sh` (or `make proto`) reproduces the
      committed Go+TS stubs with no diff — a drift guard test (`internal/loopwire/v1/drift_test.go`)
      already asserts this and skips when the toolchain is absent; run it locally with buf on PATH to
      confirm end-to-end.

## Findings

- **Real bug found + fixed during Task 5 (StartLoop context lifetime).** The handler originally
  launched loops under the per-unary-request context, which Connect cancels the instant the RPC
  returns — killing the loop before its first turn (durable history stopped at `model.call.end`).
  Fixed by giving `loopconnect.Handler` a loop-lifetime `baseCtx` (`WithBaseContext`, defaults to
  `context.Background`; the server wires its serve/shutdown ctx). Regression-tested. This is a genuine
  transport-edge lifecycle hazard worth remembering: a streaming/lifetime resource must never be tied
  to a unary request's context.
- **ADR-0033 recorded, supersedes ADR-0032.** The networked binding transport is now Connect/protobuf
  (`fuse.loop.v1`); the JSON-over-WebSocket + HTTP-replay wire and the `coder/websocket` dependency are
  removed (zero consumers). `Observe(from_seq)` subsumes the old Attach/HTTP-replay endpoint.
- **Reconcile lesson (local tree staleness).** The local checkout was ~20 commits behind `origin/main`
  and lacked #48's WS binding entirely; the feature branch correctly cut from `origin/main`, so the
  build was unaffected — but the reconcile must inspect the integration branch, never the local tree.

## Follow-ups

Not filed as stubs (auto_capture disabled this repo); surfaced here and in the run report:

- **`ForwardedScheme` is implemented + unit-tested but not yet consumed by the serving path** — no
  logging/URL-derivation hook wires it in. The plan only required forwarded-header *awareness*; wiring
  it into request logging/origin derivation is a small follow-up (natural alongside #51 observability).
- **TS workspace CI.** The connect-es smoke runs via a Go test harness (`node --import tsx`); a
  committed `npm test` job / `tsconfig` for `proto/` is not set up. #50 (the TS SDK) is the natural
  home for a first-class TS CI lane.
- **h2c vs TLS.** The connect-go server is served over h2c (plain-TCP HTTP/2) for gRPC/gRPC-Web/browser
  reach; `golang.org/x/net` was promoted from indirect. TLS termination is an ingress concern
  (out of scope, #55 owns resilience-to-conditions not the config) — but a deployment note may be
  warranted when the environment is chosen.
