---
id: 50
slug: client-sdk
title: Client SDK — Runtime-parity Go + TS/JS libraries, same API local-or-remote
status: implemented
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [48, 49, 55]
related: [45, 48, 49, 55]
discovered_from: [45]
adrs: [26, 33, 34, 35]
spec: docs/superpowers/specs/2026-08-11-client-sdk-design.md
plan: docs/superpowers/plans/2026-08-11-client-sdk-plan.md
results: docs/results/2026-08-11-client-sdk-results.md
trivial: false
auto_groomable:
branch: feat/client-sdk
claimed_at: 2026-08-11T18:36:33Z
pr: https://github.com/ethanhinson/fuse/pull/54
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-client-sdk-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-client-sdk-design.md) |
| Plan | [2026-08-11-client-sdk-plan.md](https://github.com/ethanhinson/fuse/blob/feat/client-sdk/docs/superpowers/plans/2026-08-11-client-sdk-plan.md) |
| Results | [2026-08-11-client-sdk-results.md](https://github.com/ethanhinson/fuse/blob/feat/client-sdk/docs/results/2026-08-11-client-sdk-results.md) |
| PR | [#54](https://github.com/ethanhinson/fuse/pull/54) |
| ADRs | [ADR-0026](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0026-handle-returning-spawn-seam-agent-free-interface.md), [ADR-0033](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0033-networked-binding-connect-protobuf-fuse-loop-v1.md), [ADR-0034](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0034-edge-enforced-auth-multi-tenancy-loop-ownership.md), [ADR-0035](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0035-sdk-local-backend-takes-prebuilt-runtime.md) |
<!-- docket:artifacts:end -->

## Why

The end state is that the user's other apps are **thin network clients** over a hosted fuse
service. A client SDK gives those apps a small library to import that exposes the **same API
whether the runtime is local (in-process) or remote (over the wire)** — the same surface as the
`Runtime` seam, backed by the networked binding (change 48) and its auth (change 49) when remote.
This is what makes "any AI app can target fuse" concrete: import the SDK, get a loop, local or
hosted, identically. Change 48 explicitly deferred SDK ergonomics (a REST-native surface and a
versioned client wire envelope) to this change.

The **Wander** demo app is a browser (TS/JS) client — it is the concrete reason #50 ships a **JS
SDK alongside the Go one**, and the reason the cross-language wire contract is in scope now.

## What changes

**Two SDKs** presenting the same **Runtime-parity** surface — method surface **identical to the
`Runtime` seam** (`StartLoop` / `Send` / `Observe` / `Attach`, keyed on `loopID`):

- **Go SDK** — local in-process backend **and** remote WS/HTTP backend, one constructor switch; both
  ship. The literal "same API local-or-remote" thesis.
- **TS/JS SDK** — **remote-only** (a browser has no in-process Go `Runtime`), full Runtime-parity over
  change 55's Connect transport, so the **Wander** browser app can start, drive, observe, and replay
  loops.

Both live in **this repo**: the TS SDK is a **monorepo npm workspace** (e.g. `sdk/ts`) and **Wander is
a sibling workspace package** depending on it — chosen because the proto, the Go SDK, and the TS SDK
co-evolve in lockstep right now, so a monorepo makes each proto→Go→TS→Wander change atomic and keeps
change 55's generated-stub codegen trivial; Wander still consumes the SDK through its package boundary
(exercising the real surface, not relative imports). Extraction to a standalone repo is a later option
once the wire stabilizes; a git submodule was rejected as painful for the current lockstep
co-development.

See the linked spec for the full design; at proposal altitude:

- **Versioned wire envelope comes from change 55.** A TS client and a Go server don't share types, so
  the `loop.*` envelope + `event.Event` shape must be a **versioned, schema-first contract**. That
  contract is now owned by **change 55** (the gRPC/protobuf transport, successor to change 48,
  superseding ADR-0032) — #50 **depends on 55** and generates both SDKs' clients from 55's IDL,
  rather than hand-generating TS types from Go structs over 48's JSON wire.
- **Runtime-parity, not "transparent."** The network is not invisible: both SDKs **surface** the
  event stream, the replay cursor (last `event.Seq`), gap/reconnect, and the **explicit
  park/completion event** as first-class — a client cannot infer "done" or "safe to reconnect" from
  the *shape* of a persistent loop's stream.
- **Remote client over change 55's Connect wire.** The Go SDK's remote backend and the TS SDK both
  drive change 55's Connect/protobuf transport, generating their stubs (`connect-go` / `connect-es`)
  from its proto. The reconnect discipline (subscribe-before-replay + dedup-at-watermark + gap-driven
  re-observe) is preserved over 55's server-streaming; the TS SDK reaches it in-browser via
  `connect-es` with **no proxy** (55's settled browser path). (Change 48's WS/HTTP client is superseded
  by 55, not extracted.)
- **Credential seam, pass-through now.** Each constructor takes an identity/tenant seam that forwards
  `tenant_id` present-but-unenforced today (matching change 48) and becomes the real auth carrier when
  change 49 lands — so 49 adds no breaking change to either SDK surface.

## Acceptance — the real end-to-end proof (inherited from #55's deferred smoke gap)

Change 55 shipped a **wire-contract** smoke test (real `connect-es` stub + real `connect-go` handler),
but against a **`fakeRuntime`** emitting scripted events, with a thin one-frame reconnect check that
**skips silently** when the node toolchain is absent. That proves the pipes connect, not that the
system works. #50 owns closing that gap — its acceptance MUST demonstrate:

- **A real loop, not a fake backend.** The TS SDK (via Wander, the testbed) drives a **real hosted
  loop** end-to-end — `startLoop` → `send` → `observe` → explicit completion — against the actual
  `Runtime`/engine over #55's Connect wire, not a scripted `fakeRuntime`. Live model traffic uses a
  scripted `LLM_GATEWAY_URL` double, never Claude/Anthropic (project policy).
- **Rigorous no-loss/no-dup from the TS client.** The reconnect assertion must prove the hard property
  — an event landing in the **subscribe→replay gap** is neither lost nor duplicated after
  `observe(from_seq)` re-open — from the **TS** side, not only in #55's Go resilience tests.
- **Actual browser reach.** Prove `connect-es` server-streaming + reconnect **in a real browser**
  (the manual check deferred at #55's merge gate), not just a `connect-node` client.
- **No silent-skip in CI.** The TS SDK's test lane must **fail loud** (or be a tracked, required CI
  job) rather than `t.Skip` green when the toolchain is missing — so "tests pass" cannot hide an
  unexercised TS path.

## Out of scope

- A **Python (or mobile-native) SDK** — a later change; #50 ships Go + TS/JS. The versioned envelope
  built here is what makes those cheap later.
- The **auth mechanism** (tokens / mTLS / OIDC) and enforcement — change 49; this change defines only
  the credential *seam*, in both languages.
- The **Wander app itself** — #50 ships the SDK Wander consumes, not Wander. Wander is the motivating
  consumer and acceptance target, not a deliverable here.
- Any **change to the `Runtime` seam** — the SDKs are clients *over* the seam.
- **Observability emission** (OTEL / `/metrics`) — change 51.
- **TLS / deployment topology / load-balancing** — operational concerns beneath change 48's transport.

## Open questions

- Go package home/name (`sdk/` vs `pkg/fusesdk`) — plan-time; must be importable from outside the
  module, never under `internal/`. **TS package home is decided:** a **monorepo npm workspace** in this
  repo (e.g. `sdk/ts`), with Wander as a sibling workspace package consuming it — see *What changes*.
  Bundler + publish target remain plan-time.
- **RESOLVED at reconcile (2026-08-11):** #55 shipped `fuse.loop.v1` (Connect/protobuf over h2c,
  ADR-0033) with committed generated Go stubs (`internal/loopwire/v1/loopv1connect/`) and TS stubs
  (`proto/gen/ts/`, connect-es v2 / `@connectrpc/connect`). Both SDKs consume these stubs — no wire
  or codegen is #50's job. The browser path is settled: **connect-es over HTTP, no proxy** (the
  server is h2c with an HTTP/1.1 Connect fallback; the existing `proto/smoke/client.ts` proves it).
- Exact completion-signal API shape in each language (typed event on the stream, a handle await, or
  both) — the requirement (completion observable without stream-shape inference) is fixed; the surface
  is a plan-time call. The wire carries the completion event as an ordinary `event.Event` (kind
  string, e.g. `loop.parked`) over `Observe`; the SDK surfaces it as first-class.
- **RESOLVED at reconcile (2026-08-11):** #49 shipped the credential seam as an
  `Authorization: Bearer <token>` header verified by `loopauth.Verifier` at the Connect interceptor,
  plus a `tenant` request field (spoof-rejected against the principal). Each SDK constructor takes a
  token/tenant credential provider that sets that header; Wander supplies its token the same way.

## Reconcile log

### 2026-08-11 — reconciled against merged #48/#49/#55 (transport re-map, scope shrinkage)

Reconciled #50 against current `origin/main` after all three deps (48, 49, 55) reached `done`.
This is a textbook `reconcile-transport-swapped-under-spec-remap-not-halt` case: the spec was
authored (2026-08-10) when #55 was "newly minted and undesigned," and it described the SDKs as
*generating* their clients from a not-yet-existing IDL. #55 has since landed. **All design
decisions in the spec hold on the shipped mechanism — this is a re-map, not a halt.**

**What #55 actually shipped (the SDK builds ON this, does not re-create it):**
- `proto/fuse/loop/v1/loop.proto` — service `fuse.loop.v1.LoopService`, 3 RPCs:
  `StartLoop` (unary), `Send` (unary), `Observe` (server-streaming, history-then-live). `Observe`
  *subsumes* the old separate HTTP `Attach`/replay — a reconnecting client re-opens `Observe(from_seq)`.
- Generated **Go** stubs: `internal/loopwire/v1/loopv1connect/` (connect-go); message types in
  `internal/loopwire/v1`. Generated **TS** stubs: `proto/gen/ts/fuse/loop/v1/loop_pb.ts` (connect-es
  v2 — client made at runtime via `@connectrpc/connect` `createClient`). buf toolchain under `proto/`
  (`buf.yaml`, `buf.gen.yaml`, `package.json`, `generate.sh`).
- The Connect transport edge: `internal/loopconnect/` (`Handler` over `runtime.Runtime`,
  `Observe` reconnect discipline — subscribe-before-replay + dedup-at-watermark + gap markers +
  idle keepalive). Served over h2c by `cmd/fuse/loop_serve_net.go` (`serveNet`), HTTP/1.1 Connect
  fallback also works — so the browser reaches it with **no proxy** (a #55 open question, now settled).
- An existing connect-es TS smoke client `proto/smoke/client.ts` + `internal/loopconnect/smoke_ts_test.go`.

**Transport re-map applied (spec words → shipped mechanism):**
- #48 JSON-over-WebSocket `loop.*` + separate HTTP replay → **#55 Connect/protobuf `LoopService`**;
  `Attach(loop_id, from)` HTTP endpoint → **`Observe(from_seq)` server-stream** (Attach still exists
  on the *runtime seam* for the Go local backend; on the wire it is folded into Observe).
- ADR-0032 (WS binding, cited in the spec) → **ADR-0033** (Connect transport, supersedes 0032); the
  `adrs:` field was updated 32→33 and 34 (auth) added, 26 retained.

**Scope shrinkage found (the SDK is the spec MINUS what reality already provides):**
- **No wire/IDL/codegen work.** #55 owns and ships the proto + generated Go **and** TS stubs. #50
  consumes them; it does not define the wire or run codegen as part of its deliverable.
- **Credential seam is concrete, not a placeholder.** #49 shipped it: an
  `Authorization: Bearer <token>` header verified by `loopauth.Verifier` at
  `loopconnect.NewAuthInterceptor`, plus a `tenant` request field (tenant-spoof → `CodePermissionDenied`,
  per-loop owner authz via the registry). The SDK credential seam is now "set the bearer header +
  tenant," designed against the real mechanism rather than a guessed one.
- **Runtime seam already carries tenant + ctx.** The current `Runtime` (`internal/runtime/runtime.go`)
  is `Send(ctx, tenant, loopID, input)`, `Observe(ctx, tenant, loopID) (<-chan event.Event, func(), error)`,
  `Attach(ctx, tenant, loopID, from)`. The Go **local** backend adapts to *this* signature directly
  (build an in-process `runtime.New(deps)` the way `buildLoopServerRuntimeDeps` does).

**Design decisions confirmed to still hold** (none invalidated → no halt): (1) two SDKs, Go local+remote
/ TS remote-only; (2) Runtime-parity surface; (3) versioned wire owned by #55; (4) first-class
persistent-loop mechanics — the wire carries the explicit completion event as an ordinary
`event.Event` over `Observe`, and the reconnect no-loss/no-dup discipline is real
(`internal/loopconnect/observe.go`); (5) both SDKs drive #55's transport; (6) pass-through-now
credential seam, now the real #49 mechanism.

**Acceptance section stands (inherited #55 smoke gap is real).** `internal/loopconnect/smoke_ts_test.go`
confirms #55's smoke runs against a `fakeRuntime` and `t.Skip`s silently without the node toolchain,
with a one-frame reconnect check (`smoke-over-fake-backend-proves-wire-not-system`). #50 owns closing
that: a real-loop end-to-end proof (real `Runtime`/engine over the Connect wire, scripted
`LLM_GATEWAY_URL`, never Claude — project policy), a rigorous no-loss/no-dup assertion **from the TS
client**, actual browser reach, and a loud (non-silent-skip) CI lane.

**No auto-capture:** `AUTO_CAPTURE_ENABLED=false` this repo; follow-ups (if any) reported in the run report.
