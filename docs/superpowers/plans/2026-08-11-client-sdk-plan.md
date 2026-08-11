<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0050 — Client SDK — Runtime-parity Go + TS/JS libraries, same API local-or-remote](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0050-client-sdk.md)**
<!-- docket:backlink:end -->

# Client SDK — implementation plan (change 0050)

> Plan authored by `docket-implement-next` under the **auto plan-role fallback**: the configured
> `superpowers:writing-plans` skill is not installed on this machine, so the implementer authored the
> plan directly (Skill layer missing-skill rule → degrade to auto + warn). Method is TDD; the artifact
> is this plan file, executed task-by-task on `feat/client-sdk`.

Reconciled against `origin/main` (tip 77173e7, incl. #48/#49/#55). See the change file's
`## Reconcile log` (2026-08-11): the spec's "generate from an undesigned IDL" is now a **consume the
shipped #55 stubs** task; the credential seam is the concrete #49 bearer-token mechanism; the Runtime
seam already carries `tenant`+`ctx`. This plan builds the spec **minus what #55/#49 already provide**.

## Ground truth this plan targets (all on `origin/main`)

- **Wire:** `fuse.loop.v1.LoopService` — `StartLoop`/`Send` (unary), `Observe` (server-stream,
  history-then-live; subsumes replay). Proto: `proto/fuse/loop/v1/loop.proto`.
- **Generated Go stubs:** `internal/loopwire/v1` (messages `loopv1`), `internal/loopwire/v1/loopv1connect`
  (client `NewLoopServiceClient`, handler iface). **connect-go client already generated** — the Go
  remote backend dials it, it is not re-generated.
- **Generated TS stubs:** `proto/gen/ts/fuse/loop/v1/loop_pb.ts` (connect-es v2; client built at
  runtime via `@connectrpc/connect` `createClient`). buf toolchain under `proto/`.
- **Server:** `cmd/fuse/loop_serve_net.go` `serveNet(ctx, ln, rt, verifier, registry)` over h2c
  (HTTP/1.1 Connect fallback works → **no browser proxy**). Dev token `fuse-dev-token` → `_default`.
- **Runtime seam** (`internal/runtime/runtime.go`): `StartLoop(ctx,cfg)`,
  `Send(ctx,tenant,loopID,input)`, `Observe(ctx,tenant,loopID) (<-chan event.Event, func(), error)`,
  `Attach(ctx,tenant,loopID,from) ([]event.Event, error)`.
- **Auth seam** (#49): `Authorization: Bearer <token>` at `loopconnect.NewAuthInterceptor`, plus a
  `tenant` request field (spoof-rejected). Client side = set the header + tenant field.
- **Acceptance reference harness:** `cmd/fuse/loop_serve_net_auth_test.go` (`authTestServer`,
  `newScriptedGateway`) — REAL runtime over the wire, scripted `LLM_GATEWAY_URL`, never Claude.

## Decisions settled at plan time (previously open questions)

- **D1 — Go SDK home:** `sdk/fuse` (package `fuse`) at the **module root**, import path
  `github.com/ethanhinson/fuse/sdk/fuse`. NOT under `internal/` (the whole point is external import).
  The **remote** backend imports only the generated `loopv1`/`loopv1connect` stubs +
  `internal/event` (for `event.Event`/`Seq`/`Kind` types on the public surface). The **local**
  backend imports `internal/runtime`. This keeps `sdk/fuse` a client *over* the seam.
  (`break-import-cycle-with-agent-free-subpackage`: the SDK is a leaf importer, never imported by
  `internal/*`.)
- **D2 — Go local backend input:** takes a **pre-built `runtime.Runtime`** (the spec's open
  question), not config-to-build. Building a runtime pulls the whole composition root (`cmd/fuse`
  wiring), which cannot move into an importable SDK package without dragging TUI/tools. So the local
  constructor is `NewLocal(rt runtime.Runtime, opts...)` — the caller (an app embedding fuse, or a
  test) supplies the runtime it built. This is honest: "local" = you already have an in-process
  runtime; the SDK gives it the parity surface.
- **D3 — Public surface parity:** methods mirror the seam but the SDK is the *client-facing* shape:
  `StartLoop(ctx, StartLoopConfig) (LoopID, error)`, `Send(ctx, LoopID, input) error`,
  `Observe(ctx, LoopID, fromSeq) (<-chan Event, func(), error)` — Observe **carries the reconnect
  cursor `fromSeq`** as a first-class parameter (parity with the wire; the local backend maps
  fromSeq to Attach+Observe dedup exactly as `loopconnect.Observe` does). No separate public
  `Attach` — reconnect is `Observe(fromSeq=lastSeq)`, matching the wire contract (decision 3/#55).
- **D4 — Event type on the public surface:** re-export `internal/event.Event`/`Seq`/`Kind` via SDK
  type aliases (`type Event = event.Event`) so the SDK surface is stable and callers don't import
  `internal/`. Payload stays `json.RawMessage` (SDK ergonomics = raw JSON, per the proto comment).
- **D5 — Completion signal:** surfaced as a first-class predicate `IsCompletion(Event) bool` keyed on
  the completion event kind (e.g. `event.KindLoopParked` if present, else the loop's terminal kind);
  the stream delivers it as an ordinary `Event`. Never inferred from stream shape
  (`persistent-loop-needs-explicit-completion-event`).
- **D6 — TS SDK home:** a **new npm workspace** at `sdk/ts` (package `@fuse/sdk`), a **sibling** of
  the existing `proto/` codegen package. Root `package.json` with `workspaces: ["proto","sdk/ts"]`
  (the repo has none today — this is the "JS package toolchain the repo does not have" cost). The TS
  SDK **depends on the generated stubs** in `proto/gen/ts` via a workspace/relative path, and on
  `@connectrpc/connect` + `@connectrpc/connect-web` (browser) / `@connectrpc/connect-node` (node
  tests). Wander is out of scope (its own change) — this plan ships the SDK Wander will consume, and
  proves it via a node+browser acceptance in `sdk/ts`.
- **D7 — Credential seam:** each constructor takes `{ token, tenant }` (Go: a `Credentials` struct;
  TS: a `credentials` option). The remote backend sets `Authorization: Bearer <token>` and the
  `tenant` field on every request. No auth *scheme* is hard-coded beyond bearer (the only mechanism
  #49 shipped).

## Tasks

Each task is TDD: write the failing focused test first, implement to green, run the package suite,
self-review, commit. `go test ./...` must stay green at every task boundary.

### Task 1 — Go SDK skeleton + public surface + backend interface
- **Test:** `sdk/fuse/fuse_test.go` — construct a client over a **stub backend** (a test double
  implementing the internal `backend` interface); assert `StartLoop`/`Send`/`Observe` delegate to it
  and that the public types (`Client`, `StartLoopConfig`, `Credentials`, `Event` alias, `IsCompletion`)
  exist with the D3/D4/D5 signatures.
- **Impl:** `sdk/fuse/fuse.go` — public `Client` with an unexported `backend` interface
  (`startLoop`/`send`/`observe`), `New*` constructors deferred to Tasks 2/3. Re-export event aliases.
  `IsCompletion` predicate. No network, no runtime import yet.
- **Green gate:** `go test ./sdk/...`.

### Task 2 — Go local backend (over a pre-built `runtime.Runtime`)
- **Test:** `sdk/fuse/local_test.go` — build a **real in-process runtime** the same way the net auth
  test does (scripted `LLM_GATEWAY_URL`, `buildLoopServerRuntimeDeps` is in `cmd/fuse` so instead use
  a minimal `runtime.New(deps)` with a fake/scripted model, OR a small `fakeRuntime` implementing the
  seam) and drive `StartLoop → Send → Observe`, asserting events flow and `Observe(fromSeq)` replays
  with **no loss/no dup** across a re-open (the local analogue of the wire property).
  - Note: to avoid importing `cmd/fuse` (main package, un-importable), the local backend test uses a
    **seam-level `fakeRuntime`** (mirroring `internal/loopconnect/handler_test.go`'s) that emits a
    scripted event history; the *real-engine* end-to-end proof lives at Task 6 over the wire.
- **Impl:** `sdk/fuse/local.go` — `NewLocal(rt runtime.Runtime, creds Credentials, opts...)`; each
  method forwards to the seam threading `creds.Tenant`. `Observe(fromSeq)` = subscribe-before-replay
  + `Attach(fromSeq)` + dedup-at-watermark (port `loopconnect.Observe`'s discipline into the client;
  `replay-live-handoff-dedup-at-watermark`).
- **Green gate:** `go test ./sdk/...`.

### Task 3 — Go remote backend (dials #55's connect-go client)
- **Test:** `sdk/fuse/remote_test.go` — stand up a **real `serveNet` server over a `fakeRuntime`**
  (fast wire check) via `httptest`/`net.Listen`, construct `NewRemote(baseURL, creds)`, and drive
  `StartLoop → Send → Observe`, asserting the bearer header + tenant reach the server (seed the
  verifier so a wrong token → `CodeUnauthenticated`, a tenant spoof → `CodePermissionDenied`), and
  that `Observe(fromSeq)` reconnect resumes with no loss/no dup **from the client**.
- **Impl:** `sdk/fuse/remote.go` — `NewRemote(baseURL string, creds Credentials, opts...)` using
  `loopv1connect.NewLoopServiceClient` over a `connect` HTTP client; an interceptor/`connect.Option`
  that sets `Authorization`/tenant. Map `connect.Code*` back to SDK sentinel errors
  (`ErrUnauthenticated`, `ErrPermissionDenied`, `ErrLoopNotFound`, `ErrLoopFinished`). Consume the
  `Observe` server-stream, skipping `keepalive` frames, tracking `Seq`, honoring `gap` markers.
- **Green gate:** `go test ./sdk/...` (this exercises the real Connect wire against a fake backend —
  a legitimate *wire* check per `smoke-over-fake-backend`; the real-backend proof is Task 6).

### Task 4 — TS SDK package + parity surface (node client)
- **Setup:** root `package.json` with `workspaces: ["proto","sdk/ts"]`; `sdk/ts/package.json`
  (`@fuse/sdk`, deps on `@connectrpc/connect`, `@connectrpc/connect-web`, dev `@connectrpc/connect-node`,
  `tsx`, `typescript`, a test runner — node's built-in `node:test` to avoid a heavy dep), `tsconfig.json`.
  The generated stubs are imported from `../../proto/gen/ts/...`.
- **Test:** `sdk/ts/test/remote.test.ts` (node:test) — against a **real `serveNet` server over a
  `fakeRuntime`** started by a Go helper binary/`go test` fixture (reuse the Task-3 server, exposed
  via a tiny `go run` harness or the existing smoke pattern), drive `startLoop → send → observe →
  reconnect`, asserting parity methods and **rigorous no-loss/no-dup from TS** across the
  subscribe→replay gap (stronger than #55's one-frame check).
- **Impl:** `sdk/ts/src/index.ts` — `createClient({ baseUrl, credentials })` presenting
  `startLoop`/`send`/`observe(loopId, fromSeq)`; `observe` is an async iterator over `ObserveEvent`,
  skipping keepalives, tracking `seq`, reconnecting from `lastSeq` on stream end (the browser analogue
  of `websocket-read-errors-are-not-closeerror`), surfacing `isCompletion`. Uses `connect-web`
  transport (browser) with a `connect-node` transport injectable for the node test.
- **Green gate:** `go test ./sdk/...` still green; TS test runs under node (see Task 7 CI lane).

### Task 5 — Completion-event surfacing (both SDKs), tied to real kinds
- **Test:** Go `sdk/fuse/completion_test.go` + TS assertion — feed a scripted stream ending in the
  loop's completion event; assert `IsCompletion`/`isCompletion` fires exactly on it and the caller can
  then `Send` the next turn (persistent-loop parity). Confirm the exact completion `Kind` against
  `internal/event` constants (read them, do not guess).
- **Impl:** finalize the completion predicate in both languages against the real kind.
- **Green gate:** `go test ./sdk/...`.

### Task 6 — Real-loop end-to-end acceptance (closes #55's fake-backend gap)
- **Test:** `sdk/fuse/acceptance_test.go` (Go) — stand up a **REAL runtime** over the wire using the
  scripted-gateway pattern from `cmd/fuse/loop_serve_net_auth_test.go` (a shared test helper factored
  so the SDK test can reuse it, OR replicated in-package since `cmd/fuse` is un-importable — replicate
  `newScriptedGateway` + a `runtime.New` with a scripted model). Drive the **Go remote SDK** through
  `StartLoop → Send → Observe → explicit completion` against the real engine, and assert no-loss/no-dup
  on reconnect. NEVER Claude/Anthropic — scripted `LLM_GATEWAY_URL` only (project policy).
- **Also:** the **TS** real-loop acceptance — the `sdk/ts` node test points at a real-runtime server
  (same harness) rather than the fake, proving the TS client end-to-end.
- **Green gate:** `go test ./sdk/...` green; TS acceptance green under node.

### Task 7 — Loud CI lane (no silent skip) + browser reach
- **Test/impl:** a Makefile target + CI job (`make sdk-ts-test` / a `.github` lane if present) that
  runs the TS SDK tests and **fails loud** when the node toolchain is absent in the environment that
  is supposed to have it (`smoke-over-fake-backend` rule 3). The Go `sdk` tests run in the default
  `go test ./...` lane. Document the **browser reach** as a `## Verify (human)` item in the results
  file (Task 8): a Playwright/manual run of the TS SDK's `observe` server-streaming + reconnect in a
  real browser against `connect-web` (the deferred #55 manual check), since a headless-browser CI is
  heavier than this change should add — recorded as an explicit acceptance checkbox, never left
  implicit (`smoke-over-fake-backend` war story: write the deferred proof down).
- **Green gate:** `go test ./...` green; the TS lane is invocable and loud.

### Task 8 — Docs + results close-out
- Short `sdk/README.md` (Go) + `sdk/ts/README.md` (TS): import, construct local/remote, drive a loop,
  reconnect, completion. Note the credential seam (bearer+tenant) and that auth enforcement is #49.
- A results file (Step 6.5) is **warranted** here: it carries the `## Verify (human)` browser-reach
  checkbox and records the deferred-proof + any plan deviations. Author it in the feature worktree.

## Out of scope (reaffirmed post-reconcile)
- Defining/regenerating the wire or stubs (#55 owns them; this change consumes them).
- The Wander app itself; a Python/mobile SDK; the auth *mechanism*; OTEL; TLS/topology.
- Any change to the `Runtime` seam or `internal/loopconnect`.

## Risks / notes
- `cmd/fuse` is `package main` (un-importable) → the SDK's real-runtime acceptance replicates the
  scripted-gateway + `runtime.New(deps)` wiring in-package rather than importing the composition root.
  If that wiring proves too heavy to replicate minimally, fall back to exercising the real engine via
  a `go run ./cmd/fuse loop-serve-net` subprocess with a scripted gateway (documented alternative).
- The repo has no root `package.json`/workspaces today; adding them must not disturb `proto/`'s
  existing `npm ci` flow — the `proto` workspace keeps its own `package.json`.
