<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0055 — Connect/protobuf transport — IDL-defined loop.* wire, successor to](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0055-grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4.md)**
<!-- docket:backlink:end -->

# Plan — Connect/protobuf transport (#55), successor to #48

> Plan authored inline (auto-fallback): the resolved plan role skill
> `superpowers:writing-plans` is unavailable in this environment, so per the docket
> Skill-layer missing-skill rule the plan was authored by the implementer directly.
> Method is the implementer's; the artifact (this file) and stop-point are unchanged.

Spec: `docs/superpowers/specs/2026-08-11-grpc-connect-transport-design.md` (on `docket`).
Change: #55. Base: `origin/main` (verified in reconcile — #48's WS binding, dispatch core,
and `coder/websocket` dep are all present there).

## Reconciled facts this plan builds on (from origin/main)

- **Runtime seam (untouched):** `internal/runtime/runtime.go` — `StartLoop(ctx, LoopConfig)
  (LoopHandle, error)`, `Send(ctx, tenant event.TenantID, loopID, input) error`,
  `Observe(ctx, tenant, loopID) (<-chan event.Event, func(), error)`, `Attach(ctx, tenant,
  loopID, from event.Seq) ([]event.Event, error)`. `Observe`/`Attach` already resolve a loop
  via the durable registry (cross-instance) — this is what makes the different-instance
  reconnect requirement satisfiable with NO seam change.
- **Dispatch core (reuse):** `internal/loopserver/{transport.go,server.go}` — the `conn`
  interface (`readRequest`/`encode`), the shared `req`/`resp`/`startParams`/`sendParams`/
  `observeParams`/`observeResult`/`eventNote` frame types, and `serveObserve`'s discipline:
  subscribe-before-replay → `Attach(from)` → respond watermark → replay as notifications →
  live-tail deduping `ev.Seq <= last`, gap `ev.Seq > prev+1 && prev != 0`.
- **WS binding (remove — ADR-0032 superseded):** `internal/loopserver/net.go`
  (`wsTransport`, `newWSTransport`, `ServeWS`) + `cmd/fuse/loop_serve_net.go`'s `/ws` route +
  `github.com/coder/websocket v1.8.15` dep + WS tests. The HTTP replay handler
  (`NewReplayHandler`) is a design decision at plan-time (below).
- **event.Event:** `Seq, TS, NodeID, ParentID, Depth, Turn, Kind, Payload` — NO tenant field.
  Tenant is a seam/request field. `Seq` is `uint64`; `Kind` is a string discriminant with a
  fixed enum (`turn.start`, `model.call.start`, `model.delta`, `tool.call`, `tool.result`,
  `spawn.start`, `spawn.done`, `context.summarize`, `loop.detector.trip`, `error`, …).
- **Gateway double:** tests use `httptest.NewServer` + `t.Setenv("LLM_GATEWAY_URL", srv.URL)`
  (learning `verify-tool-loop-at-gateway-seam`); NEVER Claude/Anthropic (project policy).
- **No proto toolchain yet:** no buf/protoc/connect in go.mod or on PATH; `node`/`npm` present.

## Learnings consulted (must hold in the build)

- `replay-live-handoff-dedup-at-watermark` — the acceptance test MUST force a concurrent
  append into the subscribe→replay gap; a sequential test is blind to double-delivery.
- `websocket-read-errors-are-not-closeerror` — the Connect analogue: EVERY post-open `Observe`
  stream error is a clean reconnect, never a server fault; teardown is idempotent
  (`defer cancel()` on every return path), no goroutine/subscription leak.
- `rewind-request-body-on-manual-retry` — a client retry loop over unary `StartLoop`/`Send`
  through an intermediary must reset the request body per attempt.
- `deglobalize-holder-also-per-instance-the-shared-graph` — the two-instance test must build
  TWO fully independent runtimes over ONE durable store; do not share the wired graph.
- `fanout-send-snapshot-identity-not-pointer` — any new fan-out on the Observe path snapshots
  subscriber IDs and re-validates under the lock before a non-blocking send.
- `cache-over-tenant-scoped-source-reassert-key-on-hit` — tenant stays a pass-through key on
  every RPC; never drop it from the seam call.
- `verify-from-feature-worktree-binary` — any manual verify runs the feature-worktree binary.

## Plan-time decisions (resolving the spec's open questions)

1. **Toolchain = buf, generate-and-commit.** Add `buf.yaml` + `buf.gen.yaml` under `proto/`;
   commit generated Go + TS so CI is hermetic (no live codegen). Pin plugin versions.
2. **Proto package `fuse.loop.v1`** in `proto/fuse/loop/v1/loop.proto`.
3. **Go stubs** → `internal/loopwire/v1` (module path
   `github.com/ethanhinson/fuse/internal/loopwire/v1`); connect-go handler package.
4. **TS stubs** → `proto/gen/ts/fuse/loop/v1/` — INSIDE this repo's TS workspace layout so
   change #50's TS SDK (an `sdk/ts` npm workspace, Wander a sibling) imports them as a sibling.
   (Build-time coordinator note; codegen-output location only, no scope change.)
5. **`Attach` collapses into `Observe(from_seq)`** — a single history-then-live server-stream
   is the browser-reachable shape and the recommended default; NO separate unary replay RPC and
   NO HTTP replay endpoint (the WS-era `NewReplayHandler` is removed with the WS binding — its
   catch-up role is subsumed by `Observe(from_seq)`).
6. **Go maps proto Event at the transport edge** — keep `internal/event` transport-free; a thin
   `toProtoEvent`/`fromProtoEvent` mapper lives in the connect handler package.
7. **Observe framing over HTTP/2**; keepalive via server-sent periodic heartbeat/gap frame on
   the `Observe` stream (application-level, browser-safe) with HTTP/2 pings where available.

## Proto contract (`fuse.loop.v1`)

```proto
service LoopService {
  rpc StartLoop(StartLoopRequest) returns (StartLoopResponse);   // unary
  rpc Send(SendRequest) returns (SendResponse);                  // unary
  rpc Observe(ObserveRequest) returns (stream ObserveEvent);     // server-streaming
}
message StartLoopRequest { string task = 1; string model = 2; string tenant = 3; bool interactive = 4; }
message StartLoopResponse { string loop_id = 1; }
message SendRequest { string loop_id = 1; string input = 2; string tenant = 3; }
message SendResponse {}
message ObserveRequest { string loop_id = 1; uint64 from_seq = 2; string tenant = 3; }
// ObserveEvent carries either a domain event or a keepalive/gap marker frame.
message ObserveEvent {
  Event event = 1;         // unset on a bare keepalive
  bool gap = 2;            // true when a sequence gap preceded this event (re-observe hint)
  bool keepalive = 3;      // true on an idle heartbeat frame (no event); survives idle timeout
}
message Event {
  uint64 seq = 1; google.protobuf.Timestamp ts = 2; string node_id = 3; string parent_id = 4;
  int32 depth = 5; int32 turn = 6; string kind = 7; bytes payload = 8;  // payload = raw JSON
}
```

`payload` stays `bytes` (the existing `json.RawMessage`) — the per-kind payload shapes are not
re-modeled in proto (out of scope; #50 owns SDK ergonomics). `kind` is a string to avoid
lock-stepping a proto enum with `internal/event.Kind`.

---

## Tasks (TDD — each task: failing test first, then implement, then self-review)

### Task 1 — proto toolchain + schema + committed stubs
- Add `proto/buf.yaml`, `proto/buf.gen.yaml` (connect-go + connect-es plugins, pinned),
  `proto/fuse/loop/v1/loop.proto` (contract above).
- Generate and COMMIT Go stubs → `internal/loopwire/v1/` and TS stubs → `proto/gen/ts/…`.
- Add `connectrpc.com/connect` + `google.golang.org/protobuf` to go.mod.
- **Test:** a Go test compiles against the generated `loopv1connect` client/handler
  interfaces and constructs each request/response message (proves the stubs build and the
  service shape is present). A `make proto` (or script) regenerates identically (drift guard):
  a CI-style check that `buf generate` produces no diff.
- **Verify:** `go build ./...` green; generated files compile.

### Task 2 — connect-go handler wrapping the Runtime seam (unary)
- New package `internal/loopconnect` (transport edge). Implement `loopv1connect.LoopServiceHandler`
  for `StartLoop` and `Send`, delegating to `runtime.Runtime` (map tenant string →
  `event.TenantID`, pass-through/unenforced; map `ErrLoopNotFound`/`ErrLoopFinished` to
  `connect.CodeNotFound`/`CodeFailedPrecondition`).
- Add `toProtoEvent`/`fromProtoEvent` edge mappers (keep `internal/event` transport-free).
- **Test:** table-driven handler tests with a fake `runtime.Runtime`: StartLoop returns
  loop_id; Send success + both error mappings; tenant threaded through to the seam call
  (`cache-over-tenant-scoped-source-reassert-key-on-hit`).
- **Verify:** unary RPCs round-trip against an in-process connect test server (`connect`'s
  `httptest`), no network.

### Task 3 — Observe server-stream: reattach discipline (subscribe-before-replay + dedup + gap)
- Implement `Observe` as history-then-live over the seam, porting `serveObserve`'s discipline
  VERBATIM in intent: `Observe(ctx,…)` subscribe FIRST, `Attach(from_seq)` replay, stream the
  replayed events, track watermark `last`, then live-tail dropping `ev.Seq <= last`, emitting
  `gap = ev.Seq > prev+1 && prev != 0`. `defer cancel()` on EVERY return path (leak-free).
- Connect-error-model: treat EVERY post-open stream send/context error as a clean end (return
  nil-equivalent teardown), NOT a server fault — the analogue of
  `websocket-read-errors-are-not-closeerror`.
- **Test (the load-bearing one):** drive live+replay CONCURRENTLY — force an append into the
  subscribe→replay gap and assert NO event is delivered twice and NONE is lost
  (`replay-live-handoff-dedup-at-watermark`). Assert `gap` is set on the first event after a
  forced sequence break. Assert an aborted stream context tears down with no leaked
  goroutine/subscription.
- **Verify:** `go test ./internal/loopconnect/... -race`.

### Task 4 — ingress resilience: keepalive + forwarded-header + deadline/retry
- Emit periodic `keepalive: true` `ObserveEvent` frames on an idle Observe stream (interval
  below a common gateway idle default, e.g. 20s) so a parked-but-alive loop's stream is not
  reaped; the client ignores keepalives for dedup (no Seq).
- Forwarded-header awareness: derive scheme/host from `X-Forwarded-*` when present; never
  hard-assume own TLS.
- Provide a Go client helper with a retry loop over unary calls that RESETS the request body
  per attempt (`rewind-request-body-on-manual-retry`) and is deadline-safe through an
  intermediary.
- **Test:** (a) a stream held idle past a simulated idle timeout survives via keepalive OR the
  client cleanly re-establishes from `from_seq` — assert no loss/dup across the gap; (b) a
  handler behind a `X-Forwarded-Proto: https` request logs/derives https; (c) a unary retry
  re-sends a non-empty body on attempt 2.
- **Verify:** `go test ./internal/loopconnect/... -race`.

### Task 5 — acceptance: two-instance different-instance reconnect over the durable store (#47)
- Stand up TWO independent `connect-go` servers (two fully-wired runtimes,
  `deglobalize-holder-also-per-instance-the-shared-graph`) over ONE durable store (#47:
  fsstore or pgstore). `StartLoop` on instance A; drive a turn via the scripted
  `LLM_GATEWAY_URL` double; the client opens `Observe` on A, then RE-OPENS `Observe(from_seq)`
  routed onto instance B (in-test router/toggle) and asserts B replays A's loop correctly with
  no loss/dup. Force a concurrent append into B's subscribe→replay gap to keep the dedup honest.
- **Verify:** `go test ./cmd/fuse/... -run TwoInstance -race` (gateway double only).

### Task 6 — connect-es TS client + browser-reachable smoke
- Minimal TS harness (in the committed `proto/gen/ts` workspace) that opens unary
  `StartLoop`/`Send` and the `Observe` server-stream against a running `connect-go` server,
  tracks `event.seq`, re-opens `Observe(from_seq)` on stream error. A Playwright-driven (repo
  already vendors `playwright-go`) or node-fetch smoke asserting server-streaming reaches the
  browser/runtime and reconnect replays. Keep it lean — testbed proof, not production polish.
  If a real browser harness proves too heavy for CI, assert connect-es server-streaming +
  reconnect from node with the connect-es client (still the real generated stub), and record
  the browser-manual step in the results file for the merge gate.
- **Verify:** `npm test` (or the node smoke) green against a locally-started server.

### Task 7 — remove #48's WS binding (ADR-0032 superseded)
- Delete `internal/loopserver/net.go`'s WS transport (`wsTransport`, `newWSTransport`,
  `ServeWS`) and the HTTP replay handler (`NewReplayHandler`, `writeJSONError`) — their roles
  are subsumed by the Connect `Observe(from_seq)` stream. Delete WS/HTTP tests
  (`net_test.go`, `http_test.go`). Keep `transport.go`/`stdio.go`/`server.go` (stdio binding #2
  is untouched) — but if the extracted `conn`/frame types are now used ONLY by stdio, that is
  fine (they remain the stdio transport's own types); do NOT delete anything stdio needs.
- Rewire `cmd/fuse/loop_serve_net.go`: the `loop-serve-net` subcommand now serves the
  connect-go handler (mount `loopv1connect.NewLoopServiceHandler` on the mux) instead of the
  `/ws` + `/loops/` routes. Remove the `coder/websocket` import; `go mod tidy` drops the dep.
- **Test:** `loop_serve_net_test.go` updated to dispatch/serve the connect handler over an
  ephemeral listener (reuse the `netListen`/`serveNetContext` seams). Assert no residual
  `coder/websocket` reference remains (`grep` guard in a test or CI note).
- **Verify:** `go build ./...`, `go vet ./...`, `go mod tidy` leaves no `coder/websocket`;
  full `go test ./... -race` green.

### Task 8 — subcommand help + docs + full-suite gate
- Update `cmd/fuse/main.go` help/usage for `loop-serve-net` (now Connect/protobuf).
- README/docs note: the networked binding is Connect/protobuf; `#48`'s JSON WS/HTTP wire is
  removed (ADR-0032 superseded by the new ADR).
- **Verify:** whole-suite `go test ./... -race` green; `go build ./...`.

## ADR to record (Step 6)

A new ADR that **supersedes ADR-0032**: "Networked binding transport = Connect/protobuf
(`fuse.loop.v1`), replacing the JSON-over-WebSocket + HTTP-replay wire." `supersedes: [32]`,
`change: 55`. Captures: Connect-over-gRPC for browser reach without a proxy; unary + server-
streaming only, no bidi; `Observe(from_seq)` subsumes Attach/HTTP-replay; buf generate-and-
commit; tenant pass-through as a typed field; the ingress-resilience posture (keepalive,
forwarded headers, retry/deadline safety, different-instance reconnect over #47).

## Out of scope (unchanged from spec)

SDK ergonomics (#50), auth/identity enforcement (#49), Python/mobile SDK, Runtime seam / stdio
binding #2 changes, ingress *configuration*, observability emission (#51).
