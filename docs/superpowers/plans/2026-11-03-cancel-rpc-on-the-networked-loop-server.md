<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0117 — Cancel RPC on the networked loop server](../../changes/active/0117-cancel-rpc-on-the-networked-loop-server.md)**
<!-- docket:backlink:end -->

# Cancel RPC on the networked loop server - Implementation Plan

> Executed by `docket-build`: one task at a time, each task TDD (failing test, minimal code, passing test, one commit). Every test uses fakes (fake runtime, fake registry, `fsstore` durable store in a temp dir, blocking completers) or a scripted `httptest` gateway. No live model traffic and never Anthropic. A manual live check, if anyone runs one, uses the gateway models `glm`/`kimi` through `localhost:4000`.

> Authored inline by the plan writer: `superpowers:writing-plans` is not available in this environment (missing-skill rule).

**Goal:** A caller can stop a loop it owns with a new `CancelLoop` RPC (scope `loop:cancel`, also satisfied by `loop:admin`). The loop's in-flight model call, tools, sandbox processes and children unwind through context cancellation. Its pools and MCP manager are released. A terminal `loop.cancelled` event carrying the caller's reason becomes the highest-seq event of the stream. The registry shows the loop not live before the call returns `CANCELLED`. The loop is sealed: `Send` gets `FailedPrecondition` "loop cancelled" and `Resume` refuses it. Cancel is idempotent and tenant-isolated, relays across replicas through the durable stream, and is counted by `fuse_loops_cancelled_total{tenant_id, was_live}`.

**Architecture:**
- `internal/event`: two kinds, `loop.cancel.request` (relay request, payload `{reason, requested_by}`) and `loop.cancelled` (terminal, payload `{reason, requested_by, was_live, request_seq}`).
- `internal/loopauth` + `internal/config`: `ScopeLoopCancel = "loop:cancel"`, added to the static-token vocabulary (`knownScopes`).
- `proto/fuse/loop/v1/loop.proto`: `CancelLoop` RPC, `CancelLoopRequest`, `CancelOutcome`, `CancelLoopResponse`. Regenerated with `make proto` (Go in `internal/loopwire/v1`, TS in `proto/gen/ts`).
- `internal/runtime`: optional `Canceller` capability (ADR-0047 pattern), `CancelResult`, `CancelOutcome`, `ErrLoopCancelled`, `Deps.CancelWait` (default 10s). Every loop's run context now derives from a per-loop `context.WithCancelCause` (`loop.abort`). The local cancel and the completion goroutine coordinate through two fields guarded by `r.mu` (`cancelled`, `finishing`). The completion goroutine appends `loop.cancelled` before `SetLive(false)`. A loop not in the cache is sealed by replay + append (not live) or relayed with `loop.cancel.request` (live elsewhere). `Resume` refuses by replay.
- `internal/loopconnect`: `Handler.CancelLoop`; `Send` maps `ErrLoopCancelled` to `FailedPrecondition` "loop cancelled" and never resumes a cancelled loop.
- `internal/observe` + `internal/observe/prometheus` + `cmd/fuse/observability.go`: `loop.cancelled` classifies as `OperationLoop`/`OutcomeCanceled`; a bounded `Record.CancelWasLive` drives the new counter; the projecting store wrapper also projects `AppendCommitted`.
- `internal/orchestrator`: the watcher shows `loop.cancelled` to an orchestrator.
- `sdk/ts`: `Client.cancel`, `KIND_LOOP_CANCELLED`, `isCompletion` true for it.
- Docs: README scopes table and RPC list, chart `values.yaml` scope comment, `docs/observability.md`, handler and proto doc comments. One ADR, recorded by the parent.

**Tech stack:** Go, connect-go, buf / protoc-gen-go / protoc-gen-connect-go / protoc-gen-es, Prometheus client, TypeScript (node:test, `@connectrpc/connect` router transport).

**Learnings applied:**
- `per-instance-resource-needs-teardown-on-every-early-return`: the new per-loop abort context is created after the last early return in `launchLoop` (the `BuildAgent` failure), so no early-return path can leak it, and the completion goroutine releases it. Keep it that way. The cancel path must still reach `LoopTeardown`; Task 4 asserts the hook ran.
- `security-knob-inert-at-composition-root`: a `Canceller` the handler cannot see is a silent `Unimplemented`. `cmd/fuse` has no runtime wrapper today (`runtime.New(deps)` at `cmd/fuse/loop_serve_net.go` line ~436 is handed straight to the handler). Task 10 drives the real `fuse loop-serve-net` command and asserts `CANCELLED`, not `Unimplemented`. Also: `projectingDurableStore` in `cmd/fuse/observability.go` projects only its `Append` override; its promoted `AppendCommitted` bypasses projection. The runtime appends `loop.cancelled` through `AppendCommitted` (to learn its seq), so without Task 8's override the metric would never move in the shipped binary while every unit test passes.
- `closed-label-allowlist-silently-swallows-enum-growth`: `was_live` is a closed two-value label. Task 8 tests that both `"true"` and `"false"` are admitted and anything else collapses to `__overflow__`.
- `replay-live-handoff-dedup-at-watermark`: the relay requester waits for `loop.cancelled` by subscribing first, then replaying from the request seq, and dedups live events at the replay watermark (Task 6).
- `spec-asserted-error-code-verify-emitter-first`: the spec keys the Send behaviour on `FailedPrecondition` "cancelled". Task 7 traces every emitter: a cached cancelled loop returns `ErrLoopCancelled` from `liveLoop`; a non-cached one reaches it only through the handler's `Resume` retry (`liveLoop` reports `ErrLoopFinished` for a not-live record), so the handler must surface the `Resume` error when it is `ErrLoopCancelled`. Without that, a cancelled loop on another replica answers "loop finished".
- `connect-stream-terminal-errors-stop-reconnect`: `FailedPrecondition` is already terminal in the TS SDK (`TERMINAL_CODES`), so an Observe of a cancelled loop never hot-loops. No change needed; Task 11 keeps it.
- `persistent-loop-needs-explicit-completion-event`: `loop.cancelled` is an explicit terminal fact on the wire; the TS `isCompletion` treats it like `loop.parked`.
- `scripted-gateway-double-double-escapes-tool-args`: the e2e gateway emits no tool calls (it either holds or answers text), so no escaping layer is involved. If a builder adds a tool-call reply, pass plain JSON args.
- `mutex-test-double-concurrent-provider`: every test double read by the run goroutine and written by the test (holding gateway's "saw cancel" flag, teardown counters) is mutex- or atomic-guarded on both sides.
- `httptest-defer-close-before-tcleanup-deadlock`: register gateway and server teardown with `t.Cleanup`, not `defer`, in the e2e test (the holding handler blocks until its request ctx ends).
- `reconcile-verify-claims-against-origin-not-working-tree`: anchors checked on the feature tree at `03348f7`: `launchLoop` completion goroutine order (`internal/runtime/inproc.go` ~lines 830-890), `liveLoop` (~1166), `Resume` (~1035), `startRelayPump` (`relay.go`), `requireScope`/`authorizeLoop` (`handler.go`), `knownScopes` (`internal/config/validate.go` ~301), `classify` (`internal/observe/projector.go` ~201), recorder catalog (`internal/observe/prometheus/recorder.go`).

**Decisions made in this plan (put them in the PR body):**
1. **Who decides `was_live`.** Two `loop` fields guarded by `r.mu`: `cancelled *cancelState` (set by the first cancel) and `finishing bool` (set by the completion goroutine at its decision point). A cancel that finds `finishing == false` fires `abort` and the completion goroutine appends `loop.cancelled{was_live:true}`. A cancel that finds `finishing == true` (the run already returned, or returned concurrently) appends `loop.cancelled{was_live:false}` itself. Exactly one of them appends.
2. **`CANCELLED` means not live.** The local path returns `Cancelled` only after `handle.done` is closed, which happens after `SetLive(false)` and `LoopTeardown`. If the wait (`min(ctx, Deps.CancelWait)`) expires first it returns `Cancelling`. The relay requester returns `Cancelled` only after it has seen `loop.cancelled` AND the registry record reads not live (polled every 10ms within the same deadline); otherwise `Cancelling`.
3. **Terminal seq.** The terminal append uses `event.CommittedDurableStore.AppendCommitted` when the durable store implements it (fsstore and pgstore do) and reads the seq from the committed envelope. With a plain `DurableStore` it appends and then replays to find the seq. Without a durable store it appends to the loop's own store and replays it; on any failure the seq is 0 (the spec allows 0 when the store cannot report it).
4. **Terminal append bypasses the loop's sink.** It goes straight to the durable store with `context.Background()`, not through `durableSink` (whose ctx is the session ctx, which the completion goroutine cancels). It therefore opens no store span. The projecting wrapper still projects it (Task 8).
5. **Children finish first.** Before the terminal append the completion goroutine waits, bounded (5ms poll, 5s cap), for `tree.ActiveCounts()` to report `(0, 0)` so every `spawn.done` precedes `loop.cancelled`. Only on the cancel path; a normal completion is byte-identical.
6. **Handler check order.** scope, then tenant spoof, then empty `loop_id` (`InvalidArgument`, so an empty id never reaches the registry as a NotFound), then `authorizeLoop` (NotFound / owner PermissionDenied), then `reason` validation (`InvalidArgument`), then the `Canceller` assertion (`Unimplemented`), then the call.
7. **Reason validation.** At most 256 bytes, valid UTF-8, no rune for which `unicode.IsControl` is true. Empty is allowed.
8. **Concurrent cancels.** A second cancel that finds `cancelled` already set waits for that cancel's terminal append (bounded) and returns `AlreadyCancelled` with the first reason and seq; if the terminal is not yet recorded within the wait it returns `Cancelling`. It never appends.
9. **Relay de-duplication is process-local.** `r.cancelPending map[event.StreamKey]event.Seq` (guarded by `r.mu`) holds the seq of the request this process appended. While an entry exists, a further cancel replays from that seq: `loop.cancelled` found means `AlreadyCancelled` (and the entry is dropped); otherwise `Cancelling` with no append. The entry is dropped when the requester sees `loop.cancelled`.
10. **Two `loop.cancelled` events can exist** in one narrow race (two replicas sealing the same not-live loop at once). Every reader takes the FIRST `loop.cancelled` in seq order as authoritative. The seal holds either way.
11. **A cached loop that another replica sealed.** If replica B seals a loop that finished on replica A (A still caches it), a Send on A answers "loop finished", not "loop cancelled", because A's in-memory seal is not set. The loop still never runs again: a cached loop is never relaunched by `Resume` case 1. Record this as a known limit in the PR body.
12. **No Go SDK change.** The spec scopes the SDK work to `sdk/ts`. `sdk/fuse` is untouched; mention it in the PR body as a possible follow-up.
13. **Orchestrator watcher.** `loop.cancelled` is a significant event for an orchestrator: one line with the reason, and the derived state "cancelled - it will not run again".

**ADR:** recorded by the parent through `docket-adr`, not by a build worker. See Task 13.

**Change 0118 note:** change 0118 also edits `loop.proto`. If it lands on `origin/main` first, rebase and re-run `make proto` before the PR (reconcile log of this change).

---

## Task 1: Event kinds and payloads

**Tier:** economy

**Files:**
- Modify: `internal/event/event.go`
- Modify: `internal/event/event_test.go` (the test that pins kind strings)

**Step 1: Write failing tests** in `internal/event/event_test.go`:
- Extend the existing kind-pinning test with `KindLoopCancelRequest == "loop.cancel.request"` and `KindLoopCancelled == "loop.cancelled"`.
- `TestLoopCancelledPayloadJSON`: marshal `LoopCancelledPayload{Reason: "operator stop", RequestedBy: "alice", WasLive: false}` and assert the JSON is exactly `{"reason":"operator stop","requested_by":"alice","was_live":false}` (`was_live` is always present; `request_seq` omitted when 0). With `RequestSeq: 7` the JSON contains `"request_seq":7`.
- `TestLoopCancelRequestPayloadJSON`: `LoopCancelRequestPayload{Reason: "r", RequestedBy: "bob"}` marshals to `{"reason":"r","requested_by":"bob"}`.

**Step 2:** `go test ./internal/event -count=1` fails (undefined identifiers).

**Step 3: Implement** in `internal/event/event.go`, after `KindToolIdentityMinted` in the const block:
```go
// KindLoopCancelRequest asks the replica that runs a loop to cancel it. The
// replica that received CancelLoop does not run the loop, so it writes the
// request into the loop's durable stream, the same way steer.request relays a
// message. The owner's relay pump honours it exactly like a local cancel. An
// unanswered request (no loop.cancelled after it) is as good as a cancel: a
// resume refuses the loop and seals it.
KindLoopCancelRequest Kind = "loop.cancel.request"
// KindLoopCancelled is the terminal event of a cancelled loop: the highest seq
// the loop ever appends. Every event the unwinding produced precedes it. A
// stream holding it never runs again (Send and resume refuse it). WasLive says
// whether the cancel interrupted a running or parked loop, or sealed one that
// had already finished or been idle-reaped. Never emitted by an idle reap or a
// drain: those loops stay resumable.
KindLoopCancelled Kind = "loop.cancelled"
```
Payloads next to `SteerRequestPayload`:
```go
// LoopCancelRequestPayload accompanies KindLoopCancelRequest.
type LoopCancelRequestPayload struct {
	Reason      string `json:"reason,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"` // principal subject, never a token
}

// LoopCancelledPayload accompanies KindLoopCancelled.
type LoopCancelledPayload struct {
	Reason      string `json:"reason,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"` // principal subject, never a token (ADR-0046)
	WasLive     bool   `json:"was_live"`
	// RequestSeq is the seq of the loop.cancel.request this answers; 0 for a
	// cancel made on the owning replica.
	RequestSeq uint64 `json:"request_seq,omitempty"`
}
```

**Step 4:** `go test ./internal/event -count=1` passes; `go build ./...` passes.

**Step 5: Commit** `feat(event): loop.cancel.request and loop.cancelled kinds`.

---

## Task 2: `loop:cancel` scope

**Tier:** economy

**Files:**
- Modify: `internal/loopauth/scopes.go`, `internal/loopauth/scopes_test.go`
- Modify: `internal/config/validate.go` (`knownScopes`), its test file (find the test that covers unknown static scopes: `grep -n knownScopes\|unknown scope internal/config/*_test.go`)

**Step 1: Failing tests:**
- `internal/loopauth/scopes_test.go`: `ScopeLoopCancel == "loop:cancel"`. A principal with scopes `loop:start loop:send loop:observe` does not `Allows(ScopeLoopCancel)`. An unrestricted principal does. A principal with only `loop:cancel` allows it.
- `internal/config`: a static token entry with `scopes: [loop:cancel]` validates with no problem (today it is an "unknown scope" problem).

**Step 2:** run `go test ./internal/loopauth ./internal/config -count=1`; confirm failures.

**Step 3: Implement:** add to the const block in `scopes.go`:
```go
// ScopeLoopCancel authorizes CancelLoop on a loop the caller owns. loop:admin
// also satisfies it (and lifts the owner check within the tenant). No other
// scope implies it: a start-only or send-only token cannot stop loops.
ScopeLoopCancel = "loop:cancel"
```
Add `loopauth.ScopeLoopCancel: true` to `knownScopes` in `internal/config/validate.go`.

**Step 4:** tests pass.

**Step 5: Commit** `feat(loopauth): loop:cancel scope`.

---

## Task 3: Wire contract - `CancelLoop` in `loop.proto`, regenerate, handler stub

**Tier:** economy

**Files:**
- Modify: `proto/fuse/loop/v1/loop.proto`
- Regenerated (commit them): `internal/loopwire/v1/loop.pb.go`, `internal/loopwire/v1/loopv1connect/loop.connect.go`, `proto/gen/ts/fuse/loop/v1/loop_pb.ts` (whatever `make proto` rewrites)
- Modify: `internal/loopconnect/handler.go` (a temporary `CancelLoop` that answers `Unimplemented`, so `var _ loopv1connect.LoopServiceHandler = (*Handler)(nil)` keeps compiling)
- Modify: `internal/loopconnect/handler_test.go`

**Step 1: Failing test** in `internal/loopconnect/handler_test.go`: `TestCancelLoopStubUnimplemented` calls `client.CancelLoop(ctx, connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: "l"}))` through `newTestServer(t, &fakeRuntime{})` and expects `connect.CodeUnimplemented`. It does not compile yet.

**Step 2:** confirm the compile failure: `go test ./internal/loopconnect -run TestCancelLoopStub -count=1`.

**Step 3: Implement.** In `loop.proto`, add to `service LoopService` after `Send`:
```proto
  // CancelLoop stops a loop the caller owns and seals it: its in-flight work is
  // cancelled, its resources are released, loop.cancelled is appended as the last
  // event of its stream, and the loop never accepts another Send or resume.
  // Idempotent. Same ownership rule as Send; requires loop:cancel (or loop:admin).
  // A server whose runtime cannot cancel answers Unimplemented.
  rpc CancelLoop(CancelLoopRequest) returns (CancelLoopResponse);
```
and the messages after `SendResponse`, exactly as the spec's section 1 (`CancelLoopRequest{loop_id=1, tenant=2, reason=3}`, `enum CancelOutcome {UNSPECIFIED=0, CANCELLED=1, ALREADY_CANCELLED=2, CANCELLING=3}`, `CancelLoopResponse{outcome=1, was_live=2, terminal_seq=3, reason=4}`) with the spec's field comments. Also mention CancelLoop in the package comment's RPC list at the top of the file.

Run `make proto`. It needs `buf`, `protoc-gen-go`, `protoc-gen-connect-go` on PATH and `cd proto && npm ci` done (see `proto/generate.sh`). If the toolchain is missing, return BLOCKED naming the missing tool; never hand-edit generated files.

In `handler.go` add:
```go
// CancelLoop is filled in by the cancel change's edge task; until then it
// answers Unimplemented like a server whose runtime cannot cancel.
func (h *Handler) CancelLoop(ctx context.Context, req *connect.Request[loopv1.CancelLoopRequest]) (*connect.Response[loopv1.CancelLoopResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("loopconnect: CancelLoop not implemented"))
}
```

**Step 4:** `go build ./... && go test ./internal/loopconnect ./sdk/... ./cmd/fuse -count=1` pass (`sdk/fuse` and the TS test servers embed `*loopconnect.Handler`, so they compile unchanged). `git status` shows only the proto, generated files and handler changes.

**Step 5: Commit** `feat(proto): CancelLoop RPC on LoopService`.

---

## Task 4: Runtime - `Canceller`, per-loop abort, local cancel, terminal event, seal

**Tier:** premium (the completion-goroutine ordering is the contract: the terminal event must be last, exactly one party appends it, and `CANCELLED` must imply not live; a mistake is a resumable "cancelled" loop or a double terminal)

**Files:**
- Modify: `internal/runtime/runtime.go` (capability types)
- Modify: `internal/runtime/inproc.go`
- Create: `internal/runtime/cancel.go` (the cancel logic, to keep `inproc.go` from growing further)
- Create: `internal/runtime/cancel_test.go`

**Step 1: Failing tests** in `internal/runtime/cancel_test.go`. Use a durable runtime: `fsstore.NewDurableFSStore(t.TempDir())` as both `DurableStore` and `Registry`, `RelayPoll: 20*time.Millisecond`, `CancelWait: 5*time.Second`, `IdleTTL: time.Minute`, a `LoopTeardown` hook that increments a mutex-guarded counter, and `BuildAgent` returning `agent.New(completer, execAll{reg}, nopRenderer{}, modelID, "", 1, 0)`. Reuse `completerFunc`, `execAll`, `nopRenderer`, `eventually`, `modelToolCall`, `modelText` from the existing test files. Helper `streamOf(t, store, tenant, loopID) []event.Event` replays from 0. Helper `lastKind(evs)`.
- `TestCancelOneShotDuringModelCall` (criterion 5): the completer closes a `started` channel, then blocks on `<-ctx.Done()` and records `ctx.Err()` (mutex-guarded), returning `ctx.Err()`. Start a one-shot loop under tenant `"acme"`, wait for `started`, call `rt.(Canceller).Cancel(ctx, "acme", id, "operator stop", "alice")`. Assert: `res.Outcome == CancelOutcomeCancelled`, `res.WasLive`, `res.Reason == "operator stop"`; the completer saw `context.Canceled`; `h.Wait()` returns an error that `errors.Is(err, context.Canceled)`; the registry record (`store.Resolve`) is NOT live at the moment Cancel returned (read it right after the call); the stream's last event is `loop.cancelled` with payload `{reason:"operator stop", requested_by:"alice", was_live:true, request_seq:0}`, its `Seq == res.TerminalSeq`, and an `error` event precedes it.
- `TestCancelDuringToolCall` (criterion 6): first model reply is `modelToolCall("c1", "block", "{}")`; register a test tool `blockTool` whose `Execute(ctx, _)` closes `started` and returns only after `<-ctx.Done()`. After `started`, Cancel. Assert the `tool.result` for `c1` has `is_error: true` and its output contains `cancelled`, it precedes `loop.cancelled`, and the `LoopTeardown` counter is 1 after Cancel returns.
- `TestCancelParkedInteractiveLoop` (criterion 8): interactive loop with a completer answering `modelText("hi")`; wait for `loop.parked` in the stream. Cancel. Assert `loop.parked` precedes `loop.cancelled{was_live:true}`; `rt.Send(...)` returns an error that `errors.Is(err, ErrLoopCancelled)` and NOT `ErrLoopFinished`; `rt.(Steerer).Steer(...)` likewise; `rt.Resume(...)` returns `ErrLoopCancelled` and the stream gains no new events (compare lengths); `rt.Observe` returns no error and `rt.Attach(ctx, "acme", id, 0)` returns the full history including `loop.cancelled`. Then with a runtime built with `IdleTTL: 500*time.Millisecond`, cancel a parked loop as soon as `loop.parked` appears and sleep 1s: the stream still ends with exactly one `loop.cancelled` and nothing after it (the reaper never fired a second completion).
- `TestCancelIdempotent` (criterion 9, local half): after `TestCancelOneShotDuringModelCall`'s cancel, a second `Cancel` with reason `"again"` returns `CancelOutcomeAlreadyCancelled`, `Reason == "operator stop"`, the same `TerminalSeq`, and the stream length is unchanged.
- `TestCancelFinishedLoopSeals` (criterion 9): a one-shot loop answering `modelText("done")`; `h.Wait()`. Cancel returns `Cancelled{WasLive:false}`; the stream ends with `loop.cancelled{was_live:false}`; `rt.Send` returns `ErrLoopCancelled`, not `ErrLoopFinished`; `rt.Resume` returns `ErrLoopCancelled`.
- `TestCancelIdleReapedLoopSeals` (criterion 9): interactive loop with `IdleTTL: 50*time.Millisecond`; wait until `h.Wait()` returns (reaped). Assert no `loop.cancelled` in the stream (a reap emits nothing). Cancel returns `Cancelled{WasLive:false}`; stream ends with `loop.cancelled{was_live:false}`; `Send` is `ErrLoopCancelled`.
- `TestCancelUnknownAndWrongTenant`: `Cancel(ctx, "acme", "nope", ...)` and `Cancel(ctx, "globex", <acme loop id>, ...)` both return `errors.Is(err, ErrLoopNotFound)`, and the acme loop's stream gains nothing.
- `TestCancelWaitExpiresReturnsCancelling`: `CancelWait: 20*time.Millisecond`, a completer that ignores ctx for 300ms after it starts (sleeps, then returns ctx.Err()). Cancel returns `CancelOutcomeCancelling`, `TerminalSeq == 0`; `eventually` the stream ends with `loop.cancelled{was_live:true}`.
- `TestDrainAndReapEmitNoTerminal` (criterion 11): an interactive loop drained by `Shutdown` produces no `loop.cancelled`, and neither does an interactive loop reaped by a short `IdleTTL`. `Shutdown` still does not cancel one-shot loops: that is covered by the existing drain tests (`TestShutdownHandsLoopsOff`, the `cmd/fuse` drain tests), which must keep passing unchanged.

**Step 2:** `go test ./internal/runtime -run 'TestCancel|TestDrainAndReap' -count=1` fails (no `Canceller`).

**Step 3: Implement.**

`runtime.go`:
```go
// CancelOutcome says what a Cancel did. The values line up with the wire enum.
type CancelOutcome int

const (
	CancelOutcomeUnspecified CancelOutcome = iota
	// CancelOutcomeCancelled: this call sealed the loop.
	CancelOutcomeCancelled
	// CancelOutcomeAlreadyCancelled: an earlier cancel sealed it.
	CancelOutcomeAlreadyCancelled
	// CancelOutcomeCancelling: the cancel is recorded but its terminal event has
	// not been seen yet (another replica owns the loop, or the unwind is slow).
	CancelOutcomeCancelling
)

// CancelResult reports one Cancel.
type CancelResult struct {
	Outcome     CancelOutcome
	WasLive     bool      // the cancel interrupted a running or parked loop
	TerminalSeq event.Seq // seq of loop.cancelled; 0 when not yet known
	Reason      string    // the recorded reason (the first one on AlreadyCancelled)
}

// Canceller is an optional Runtime capability (ADR-0047): stop a loop and seal
// it. A binding type-asserts for it. Errors: ErrLoopNotFound for an unknown or
// wrong-tenant loop.
type Canceller interface {
	Cancel(ctx context.Context, tenant event.TenantID, loopID, reason, requestedBy string) (CancelResult, error)
}
```
`inproc.go`:
- `var ErrLoopCancelled = errors.New("runtime: loop cancelled")` next to `ErrLoopFinished`, documented as distinct from it (a cancelled loop is never resumed).
- `Deps.CancelWait time.Duration` (doc: how long Cancel waits for a local unwind or a relayed cancel; zero means 10s) plus `defaultCancelWait = 10 * time.Second` and `func (r *inProcRuntime) cancelWait() time.Duration`.
- `inProcRuntime` gains `cancelPending map[event.StreamKey]event.Seq` (Task 6 uses it; initialise lazily under `r.mu`).
- `loop` gains:
```go
	// abort cancels this loop's run with a cause (ErrLoopCancelled). Every loop,
	// one-shot or interactive, has one. Only Cancel fires it: the idle reaper and
	// Shutdown keep using cancel (the session cancel, no cause).
	abort context.CancelCauseFunc
	// cancelled is the first cancel's record; nil until a cancel lands. Guarded
	// by inProcRuntime.mu.
	cancelled *cancelState
	// finishing is set (under inProcRuntime.mu) by the completion goroutine when
	// it decides whether to append loop.cancelled. A cancel that finds it set
	// appends the terminal event itself (was_live=false).
	finishing bool
```
and in `cancel.go`:
```go
type cancelState struct {
	reason, requestedBy string
	requestSeq          event.Seq
	wasLive             bool
	terminalSeq         event.Seq     // guarded by inProcRuntime.mu
	sealed              chan struct{} // closed once loop.cancelled is appended (or the append failed)
}
```
- In `launchLoop`, right before building `lp` (after the last early return, the `BuildAgent` failure): `abortCtx, abort := context.WithCancelCause(sessionCtx)`. Put `abort` in the `lp` literal, and build `h` before inserting `lp` into `r.loops` (set `lp.handle = h` in the literal too) so a concurrent Cancel never sees a nil `abort` or handle. `runCtx` derives from `abortCtx` instead of `sessionCtx` (`r.deps.LoopContext(abortCtx, loopCfg)` when set). The durable sink keeps `sessionCtx`, so events emitted while unwinding are not cancelled. The lease renewer, relay pump and idle reaper keep `sessionCtx`.
- Completion goroutine, after `close(stopReap)` and BEFORE `sessionCancel()` / `SetLive(false)` / store `Close` / `LoopTeardown`:
```go
		r.mu.Lock()
		lp.finishing = true
		cs := lp.cancelled
		r.mu.Unlock()
		if cs != nil {
			r.awaitChildren(tree)          // Decision 5; add it in Task 5, a no-op stub here is fine
			seq := r.appendTerminal(lp, cs) // Decision 3/4
			r.mu.Lock()
			cs.terminalSeq = seq
			r.mu.Unlock()
			close(cs.sealed)
		}
		abort(nil) // release the abort context's resources
```
`cs.wasLive` was set to true by the cancel that found `finishing == false`, so the payload says `was_live:true`.
- `func (r *inProcRuntime) appendTerminal(lp *loop, cs *cancelState) event.Seq` builds `event.Event{NodeID: lp.id, Kind: event.KindLoopCancelled, Payload: json(LoopCancelledPayload{cs.reason, cs.requestedBy, cs.wasLive, uint64(cs.requestSeq)})}` and appends per Decision 3 with `context.Background()`. Put the store-level part in `func (r *inProcRuntime) appendDurableTerminal(ctx, key, ev) (event.Seq, error)` because Task 6 reuses it for loops not in the cache.
- `Cancel` (in `cancel.go`) - local path only in this task; when `loopCache(tenant, loopID)` misses return `ErrLoopNotFound` for now (Task 6 replaces the miss branch):
```go
func (r *inProcRuntime) Cancel(ctx context.Context, tenant event.TenantID, loopID, reason, requestedBy string) (CancelResult, error) {
	if lp := r.loopCache(tenant, loopID); lp != nil {
		return r.cancelLocal(ctx, lp, reason, requestedBy, 0)
	}
	return CancelResult{}, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
}
```
`cancelLocal(ctx, lp, reason, requestedBy, requestSeq)` implements Decisions 1, 2 and 8:
  1. Lock `r.mu`. If `lp.cancelled != nil`: unlock, wait for `cs.sealed` bounded by `ctx` and `cancelWait()`; if sealed return `AlreadyCancelled{WasLive: cs.wasLive, TerminalSeq, Reason: cs.reason}` (read the seq under `r.mu`), else `Cancelling{Reason: cs.reason}`.
  2. Else create `cs`; `lp.cancelled = cs`; `live := !lp.finishing`; `cs.wasLive = live`; unlock.
  3. `live`: `lp.abort(ErrLoopCancelled)`, then wait for `lp.handle.done` bounded by `ctx` and `cancelWait()`. Done: return `Cancelled{WasLive:true, TerminalSeq: cs.terminalSeq (under r.mu), Reason}`. Expired: `Cancelling{WasLive:true, Reason}`.
  4. Not live: append the terminal immediately (`appendTerminal`), record the seq, close `cs.sealed`, then wait for `lp.handle.done` bounded the same way (normally already closed). Return `Cancelled{WasLive:false, TerminalSeq, Reason}` (or `Cancelling` if `done` did not close in time).
  Wait helper: `waitFor(ctx, ch, d) bool` using a `time.NewTimer(d)` and `select`.
- Seal:
  - `liveLoop`: after the cache hit, read `lp.cancelled` under `r.mu`; non-nil returns `fmt.Errorf("%w: %q", ErrLoopCancelled, loopID)` BEFORE the `handle.done` check. Update the doc comments on `liveLoop`, `Send` and `Runtime.Send` to list `ErrLoopCancelled`.
  - `Resume` case 1: a cached loop with `lp.cancelled != nil` returns `ErrLoopCancelled` (no touch, no launch).
  - The orchestrate-target check in `launchLoop` already goes through `liveLoop`, so a cancelled target is refused there.
- `var _ Canceller = (*inProcRuntime)(nil)` in `cancel.go`.

**Step 4:** `go test ./internal/runtime -count=1` and `go test -race ./internal/runtime -run 'TestCancel|TestShutdown|TestInteractive|TestSteer' -count=1` pass. All pre-existing runtime tests pass unchanged.

**Step 5: Commit** `feat(runtime): Canceller - per-loop abort, terminal loop.cancelled, sealed loops`.

---

## Task 5: Runtime - children unwind before the terminal event

**Tier:** standard

**Files:**
- Modify: `internal/runtime/cancel.go` (`awaitChildren`)
- Modify: `internal/runtime/cancel_test.go`

**Step 1: Failing test** `TestCancelWhileChildRunning` (criterion 7). In `BuildAgent`, register a test tool `spawnChild` whose `Execute(ctx, args)` builds `agent.NewSpawner(agent.WithTree(tree), agent.WithNode(tree.Node(tree.RootID())), agent.WithSpawnDepth(0), agent.WithEventStore(store), agent.WithChildBuilder(cb))`, calls `Spawn(ctx, agent.SpawnOpts{Label: "kid", Task: "wait"})` and returns after `h.Wait()`. The child builder `cb` (an `agent.ChildBuilder`: `func(ctx, opts, node, tree) (string, error)`) closes `childStarted` and blocks on `<-ctx.Done()`, returning `ctx.Err()`. The root completer returns `modelToolCall("c1", "spawn_child", "{}")` first. After `childStarted`, Cancel. Assert: a `spawn.done` whose payload `err` is non-empty precedes `loop.cancelled` in the stream; `tree.Scheduler().Snapshot().SlotsInUse == 0` and `tree.ActiveCounts()` is `(0, 0)` after Cancel returns (capture `tree` from the `BuildAgent` closure under a mutex).

**Step 2:** run it; with the Task 4 stub it either passes by luck or fails on ordering. Run it with `-count=20` to see whether `spawn.done` can land after `loop.cancelled`.

**Step 3: Implement** `awaitChildren(tree *agent.AgentTree)`: poll `tree.ActiveCounts()` every 5ms until `(0, 0)` or 5s elapse. Before relying on it, read `internal/agent/spawn.go` around the async completion goroutine (~line 496) and confirm that `spawn.done` is emitted BEFORE the child node leaves running/pending. If the status flips first, key the wait on the tree's own done signal for each non-root node instead (whatever the spawner exposes), and say so in the commit message.

**Step 4:** `go test ./internal/runtime -run TestCancelWhileChildRunning -count=50` passes every time; `go test -race ./internal/runtime -count=1` passes.

**Step 5: Commit** `feat(runtime): cancelled loop waits for its children before the terminal event`.

---

## Task 6: Runtime - loops not in the cache, cross-replica relay, resume refusal

**Tier:** premium (cross-replica correctness: a missed request, a double append, or a resume that ignores an unanswered request revives a cancelled loop)

**Files:**
- Modify: `internal/runtime/cancel.go`, `internal/runtime/relay.go`, `internal/runtime/inproc.go` (`Resume`), `internal/runtime/turnspan.go` (`consumedTurns`)
- Modify: `internal/runtime/cancel_test.go`, `internal/runtime/relay_test.go`, `internal/runtime/reconstruct_test.go` (or the existing file that tests `consumedTurns`/`reconstructMessages`)

**Step 1: Failing tests** (use `twoReplicas` from `relay_test.go`; extend it, or add a `twoReplicasWith(t, f, nowB, mutate func(*Deps))` variant, so tests can set `CancelWait`):
- `TestCancelOnNonOwnerRelays` (criterion 10): `f := &steerFake{targetBlock: make(chan struct{})}`; start an interactive loop on A; `<-f.targetBlock`. `resB, err := rtB.(Canceller).Cancel(ctx, DefaultTenant, id, "stop", "alice")`. Assert `resB.Outcome == CancelOutcomeCancelled`, `resB.WasLive`, `resB.TerminalSeq` equals the seq of the stream's last event, which is `loop.cancelled{request_seq: <seq of the loop.cancel.request>}`; exactly one `loop.cancel.request` exists, payload `{reason:"stop", requested_by:"alice"}`; the registry record is not live when Cancel returns; `rtA.Send` and `rtB.Send` both fail (`ErrLoopCancelled` on A; on B, `ErrLoopFinished` from `liveLoop`, and `rtB.Resume` then returns `ErrLoopCancelled`).
- `TestCancelRelayPendingAppendsOnce` (criterion 10, last sentence): register a fake live record directly in the store for a loop no runtime runs (`store.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: "ghost", Live: true, LeaseExpiry: time.Now().Add(time.Hour)})`). With `CancelWait: 30*time.Millisecond` on B: the first `Cancel` returns `Cancelling` with `TerminalSeq == 0`; a second returns `Cancelling` too; the stream holds exactly one `loop.cancel.request`. Then append `loop.cancelled` by hand to the store; a third `Cancel` returns `AlreadyCancelled` with that event's reason and seq.
- `TestResumeRefusesUnansweredCancelRequest` (criterion 10): start an interactive loop on A, wait for `loop.parked`, `Shutdown` A (record not live, no terminal). Append a `loop.cancel.request{reason:"r", requested_by:"bob"}` to the store by hand. `rtB.Resume` returns `ErrLoopCancelled`, launches nothing (record stays not live), and the stream's last event is `loop.cancelled{was_live:false, request_seq:<request seq>, reason:"r", requested_by:"bob"}`. A second `Resume` returns `ErrLoopCancelled` and appends nothing.
- `TestResumeRefusesCancelledStream`: same, but the stream already ends with `loop.cancelled`: `Resume` refuses and appends nothing.
- `TestCancelNotLiveOnColdReplica` (criterion 9, cross-process half): a one-shot loop finishes on A; `rtB.Cancel` returns `Cancelled{WasLive:false}` with a `TerminalSeq`; a repeat on B returns `AlreadyCancelled` with the same seq and appends nothing; `rtB.Resume` is `ErrLoopCancelled`.
- `TestCancelColdWrongTenant`: B cancelling A's default-tenant loop under tenant `"globex"` is `ErrLoopNotFound` and appends nothing.
- `TestOwnerRelayPumpHonoursRequestAfterResume`: a request appended while the owner is starting (append it, then start the pump via a fresh loop on A that already has it pending, simplest by calling `pendingCancel` directly) - at minimum a pure unit test of `pendingCancel(evs)`: returns the first `loop.cancel.request` with no `loop.cancelled` at a higher seq, nil when answered, nil when absent.
- Fold helpers ignore the new kinds: `consumedTurns` of `[parked, cancel.request, cancelled]` equals `consumedTurns` of `[parked]`; `reconstructMessages` output is unchanged by appending both kinds to a fixture stream; `pendingRequests` ignores `loop.cancel.request`; `harnessFromEvents` ignores both.

**Step 2:** run `go test ./internal/runtime -run 'TestCancel|TestResumeRefuses|TestOwnerRelay|Consumed|Reconstruct' -count=1`; confirm failures.

**Step 3: Implement.**
- `cancel.go` helpers:
  - `firstCancelled(evs []event.Event) (event.Event, event.LoopCancelledPayload, bool)` (Decision 10: first one wins).
  - `pendingCancel(evs []event.Event) (event.Event, event.LoopCancelRequestPayload, bool)`: the first request with no `loop.cancelled` at a higher seq.
- `Cancel`'s cache-miss branch:
  1. `rec, key, err := r.resolveDurableForDelivery(ctx, tenant, loopID)` (an expired lease is reaped to not live, never re-owned). Error: return it (`ErrLoopNotFound` for unknown/wrong tenant). `r.deps.DurableStore == nil`: return `ErrLoopNotFound`.
  2. Not live: `evs := DurableStore.Replay(ctx, key, 0)`. `firstCancelled` found: `AlreadyCancelled{WasLive: p.WasLive, TerminalSeq: e.Seq, Reason: p.Reason}`. Else append `loop.cancelled{reason, requestedBy, was_live:false, request_seq: pendingCancel seq or 0}` with `appendDurableTerminal` and return `Cancelled{WasLive:false, TerminalSeq, Reason: reason}`. (When answering a pending request, keep the CALLER's reason and requestedBy: this caller sealed it.)
  3. Live with a fresh lease (owned elsewhere): `r.relayCancel(ctx, key, reason, requestedBy)`.
- `relayCancel` (Decisions 2 and 9):
  1. Under `r.mu`: if `cancelPending[key]` exists, unlock, replay from that seq; `firstCancelled` found: drop the entry, return `AlreadyCancelled`; else return `Cancelling{Reason: reason}`.
  2. Else append `loop.cancel.request` (prefer `AppendCommitted` to learn its seq; with a plain `DurableStore`, append then replay to find it). Record `cancelPending[key] = seq`.
  3. Subscribe to `DurableStore.Subscribe(ctx, key)` FIRST, then `Replay(ctx, key, reqSeq)` (Replay returns events with `Seq > from`, and `loop.cancelled` always follows the request); track the replay watermark and ignore live events with `Seq <=` it. Wait, bounded by `ctx` and `cancelWait()`, for a `loop.cancelled`. Then poll `Registry.Resolve` every 10ms until `!rec.Live` within the same deadline.
  4. Both seen: drop the pending entry, return `Cancelled{WasLive: payload.WasLive, TerminalSeq, Reason: payload.Reason}`. Otherwise `Cancelling{Reason: reason}` (entry kept).
- `relay.go`:
  - `startRelayPump` gains `onCancel func(seq event.Seq, p event.LoopCancelRequestPayload)`. First pass: besides `pendingRequests`, call `onCancel` for `pendingCancel(evs)` when present. Later passes: call `onCancel` for each `KindLoopCancelRequest`. The wake condition becomes `ev.Kind == KindSteerRequest || ev.Kind == KindLoopCancelRequest`. A malformed payload is skipped.
  - In `launchLoop`, pass `func(seq, p) { go r.cancelLocal(context.Background(), lp, p.Reason, p.RequestedBy, seq) }` (in a goroutine: `cancelLocal` waits on `handle.done`, and the pump must not block its own loop). `cancelLocal` is idempotent, so a second request is a no-op `AlreadyCancelled` (the spec's "pump ignores further requests once one is being honoured"). `cs.requestSeq` flows into the terminal payload's `request_seq`. `lp` must exist before the pump starts (it does: the pump starts after `r.loops[rootID] = lp`).
  - Update the file comment to describe the cancel relay.
- `Resume` case 2, right after the replay and before `reconstructMessages`: `firstCancelled` found → `ErrLoopCancelled`. `pendingCancel` found → append `loop.cancelled{was_live:false, request_seq, reason: request's reason, requested_by: request's requested_by}` via `appendDurableTerminal`, then `ErrLoopCancelled`. Update the `Runtime.Resume` doc comment.
- `consumedTurns`: skip `KindLoopCancelRequest` and `KindLoopCancelled` (they are not an interrupted exchange). `reconstructMessages`, `pendingRequests` and `harnessFromEvents` already ignore unknown kinds; the tests pin it.

**Step 4:** `go test ./internal/runtime -count=1` and `go test -race ./internal/runtime -count=1` pass.

**Step 5: Commit** `feat(runtime): cancel loops owned elsewhere or finished; resume refuses cancelled loops`.

---

## Task 7: Connect edge - `Handler.CancelLoop` and Send mapping

**Tier:** premium (the user-visible authorization contract: scope, tenant isolation and owner checks must hold before any runtime call)

**Files:**
- Modify: `internal/loopconnect/handler.go`
- Create: `internal/loopconnect/cancel_test.go`
- Modify: `internal/loopconnect/handler_test.go` (remove `TestCancelLoopStubUnimplemented`; the Unimplemented case moves to the new file)

**Step 1: Failing tests** in `cancel_test.go`. Add a `cancelRuntime` double that embeds `*fakeRuntime` and implements `runtime.Canceller`, recording `(tenant, loopID, reason, requestedBy)` and the call count, returning a configurable `(runtime.CancelResult, error)`.
- `TestCancelLoopScopes` (criterion 2), using `newAuthzServer` with `cancelRuntime` and `reg.seed("acme", "loop-1", "alice", "node-1")`:
  - token `{acme, alice, Scopes: loop:cancel}` cancels: response `outcome == CANCEL_OUTCOME_CANCELLED`, `was_live`, `terminal_seq`, `reason` mapped from the runtime result; the runtime saw tenant `acme`, reason, and `requestedBy == "alice"`.
  - token `{acme, alice, Scopes: loop:send loop:observe loop:start}` gets `PermissionDenied` whose message contains `loop:cancel`; the runtime was not called.
  - token `{acme, bob, Scopes: loop:admin}` cancels alice's loop.
  - token `{acme, bob, Scopes: loop:cancel}` on alice's loop is `PermissionDenied` (owner check); runtime not called.
  - unrestricted token `{acme, alice}` cancels.
- `TestCancelLoopTenantIsolation` (criterion 3, handler half): `{other, carol}` on the acme loop is `NotFound`; a wire `tenant: "globex"` with an acme principal is `PermissionDenied`; runtime never called in either case.
- `TestCancelLoopValidation` (criterion 4): empty `loop_id` is `InvalidArgument`; a 257-byte reason is `InvalidArgument`; a 256-byte reason passes; a reason containing `"\n"` or `"\x07"` is `InvalidArgument`; invalid UTF-8 is `InvalidArgument`; runtime never called on a refusal.
- `TestCancelLoopUnimplemented` (criterion 4): `newTestServer(t, &fakeRuntime{})` (no Canceller) answers `Unimplemented`.
- `TestCancelLoopErrorMapping`: runtime returns `ErrLoopNotFound` → `NotFound`; `ErrLoopCancelled` → `FailedPrecondition`; other error → `Internal`. Outcomes `AlreadyCancelled` and `Cancelling` map to the wire enum values.
- `TestCancelLoopTraceHeader`: with an observer that yields a trace id (copy the pattern the Send trace-header test uses; `grep -n Fuse-Trace-Id internal/loopconnect/*_test.go`), the response carries `Fuse-Trace-Id`, and the api span is named `cancel_loop`.
- `TestSendCancelledLoopIsFailedPrecondition`: `fakeRuntime.sendErr = fmt.Errorf("%w: %q", runtime.ErrLoopCancelled, "l")`: Send is `FailedPrecondition`, its message contains `cancelled` and not `finished`, and `Resume` was NOT called (`resumed == 0`, `sawResumeLoop == ""`).
- `TestSendFinishedThenResumeCancelledSurfacesCancelled`: `sendErr = ErrLoopFinished`, `resumeErr = runtime.ErrLoopCancelled`: Send is `FailedPrecondition` containing `cancelled`.
- `TestStartLoopCancelledOrchestrateTargetIsFailedPrecondition`: `startErr = fmt.Errorf("runtime: orchestrate target: %w", runtime.ErrLoopCancelled)` → `FailedPrecondition`.

**Step 2:** `go test ./internal/loopconnect -run 'Cancel' -count=1` fails.

**Step 3: Implement** in `handler.go`:
- Replace the stub with `CancelLoop`, following `Send`'s shape: open the `fuse.api.request` span `cancel_loop`; `requireScope(ctx, loopauth.ScopeLoopCancel, loopauth.ScopeLoopAdmin)`; `effectiveTenant`; empty `LoopId` → `InvalidArgument`; `authorizeLoop`; `validCancelReason(m.Reason)` (Decision 7) → `InvalidArgument` "loopconnect: reason must be at most 256 bytes of printable UTF-8"; `c, ok := h.rt.(runtime.Canceller)` else `Unimplemented`; `res, err := c.Cancel(ctx, tenant, m.LoopId, m.Reason, p.Subject)`; map errors (`ErrLoopNotFound` → NotFound, `ErrLoopCancelled` → FailedPrecondition, else Internal); build `CancelLoopResponse{Outcome: cancelOutcomeToProto(res.Outcome), WasLive, TerminalSeq: uint64(res.TerminalSeq), Reason}`; set `Fuse-Trace-Id`. Every refusal sets `apiOutcome` like the other RPCs. Check order per Decision 6.
- `Send`: in the error switch add `errors.Is(err, runtime.ErrLoopCancelled)` → `FailedPrecondition` BEFORE the `ErrLoopFinished` case. In the resume branch: `if _, rerr := h.rt.Resume(...); rerr == nil { ... } else if errors.Is(rerr, runtime.ErrLoopCancelled) { err = rerr }`. Update the comment there: a cancelled loop is never resumed.
- `StartLoop`: add `runtime.ErrLoopCancelled` to the `FailedPrecondition` condition for an orchestrate target.
- Doc comments: the package comment's RPC list and the `Handler` comment ("CancelLoop needs loop:cancel; an explicitly granted loop:admin also satisfies it and lifts the owner check").

**Step 4:** `go test ./internal/loopconnect -count=1` and `go test -race ./internal/loopconnect -count=1` pass.

**Step 5: Commit** `feat(loopconnect): CancelLoop RPC; Send refuses cancelled loops`.

---

## Task 8: Observability - classification, counter, projection of committed appends

**Tier:** standard

**Files:**
- Modify: `internal/observe/projector.go`, `internal/observe/projector_test.go`
- Modify: `internal/observe/prometheus/recorder.go`, `internal/observe/prometheus/recorder_test.go`
- Modify: `cmd/fuse/observability.go` (`projectingDurableStore`), and its test (find with `grep -ln projectingDurableStore cmd/fuse/*_test.go`; add one if none)

**Step 1: Failing tests:**
- `projector_test.go`: `ProjectEvent(key, loop.cancelled{was_live:true, reason:"secret reason"})` has `Operation == OperationLoop`, `Outcome == OutcomeCanceled`, `ErrorCategory == ErrorCategoryNone`, `CancelWasLive == "true"`; `was_live:false` gives `"false"`; the marshalled Record contains neither `secret reason` nor `requested_by`. `loop.cancel.request` projects with the same `Operation`/`Outcome`/`ErrorCategory` as `steer.request` and an empty `CancelWasLive`. Every other kind keeps an empty `CancelWasLive`.
- `recorder_test.go`: projecting that record for tenant `acme` (admitted by the policy fixture the other tests use) increments `fuse_loops_cancelled_total{tenant_id="acme",was_live="true"}` to 1; `"false"` gets its own series; a Record with `CancelWasLive: "maybe"` lands in `was_live="__overflow__"`; `Catalog()` contains `{"fuse_loops_cancelled_total", "counter", ["tenant_id","was_live"]}` and `ValidateLabels` accepts it. A record with no `CancelWasLive` touches nothing.
- `cmd/fuse`: a `projectingDurableStore` over an fsstore durable store with a recording projection: `AppendCommitted` enqueues exactly one projected record with the committed seq, and `Append` still enqueues exactly one (no double projection, since `Append` delegates to `AppendCommitted`).

**Step 2:** run `go test ./internal/observe/... ./cmd/fuse -run 'Project|Cancel|Committed' -count=1`; confirm failures.

**Step 3: Implement:**
- `Record` gains `CancelWasLive string \`json:"cancel_was_live,omitempty"\`` with a comment: bounded `"true"|"false"`, set only from `loop.cancelled`; reason and requester are never projected.
- `classify`: `case event.KindLoopCancelled: return OperationLoop, OutcomeCanceled, ErrorCategoryNone`. `case event.KindSteerRequest, event.KindLoopCancelRequest: return OperationLoop, OutcomeSuccess, ErrorCategoryNone` (explicit; same as today's default for steer.request).
- `ProjectEvent`: for `KindLoopCancelled`, decode only `struct{ WasLive bool \`json:"was_live"\` }` and set `CancelWasLive` to `strconv.FormatBool`. A decode error leaves it empty.
- Recorder: catalog entry `{"fuse_loops_cancelled_total", "counter", []string{"tenant_id", "was_live"}}` with a comment (reason is free text and never a label); field `loopsCancelled *client.CounterVec` created with Help "Loops stopped by CancelLoop, by whether the cancel interrupted live work."; append to `collectors`; `cancelWasLive = map[string]bool{"true": true, "false": true}`; in `Project`: `if rec.CancelWasLive != "" { t := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_loops_cancelled_total"); r.loopsCancelled.WithLabelValues(t, boundedLabel(cancelWasLive, rec.CancelWasLive)).Inc() }`.
- `projectingDurableStore`: add
```go
// AppendCommitted projects too. The runtime appends some events (loop.cancelled)
// through AppendCommitted to learn their seq; without this override they would
// bypass projection, and every metric derived from them would stay at zero.
func (s projectingDurableStore) AppendCommitted(ctx context.Context, key event.StreamKey, e event.Event) (event.Event, error) {
	committed, err := s.CommittedDurableStore.AppendCommitted(ctx, key, e)
	if err != nil {
		return committed, err
	}
	if s.projection != nil {
		s.projection.Enqueue(observe.ProjectEvent(key, committed))
	}
	return committed, nil
}
```
and make `Append` call `s.AppendCommitted` and drop its own enqueue, so each append projects once.

**Step 4:** `go test ./internal/observe/... ./cmd/fuse -count=1` and `make observability-validate` pass (the validator reads `Catalog()`; no dashboard panel is added).

**Step 5: Commit** `feat(observe): fuse_loops_cancelled_total and loop.cancelled projection`.

---

## Task 9: Orchestrator watcher shows `loop.cancelled`

**Tier:** economy

**Files:**
- Modify: `internal/orchestrator/watcher.go` and its test file

**Step 1: Failing tests:** `significant(event.KindLoopCancelled)` is true; `oneLine` of a `loop.cancelled{reason:"operator stop"}` contains `loop.cancelled` and `operator stop`; `state` of a ring whose last significant kind is `loop.cancelled` is `"cancelled - it will not run again"`.

**Step 2:** `go test ./internal/orchestrator -count=1` fails.

**Step 3: Implement:** add `event.KindLoopCancelled` to `significant`; a `oneLine` case decoding `LoopCancelledPayload` and rendering `head + ": " + clip(p.Reason)` (just `head` when the reason is empty); a `state` case returning `"cancelled - it will not run again"`.

**Step 4:** tests pass.

**Step 5: Commit** `feat(orchestrator): watcher reports a cancelled target`.

---

## Task 10: Composition root - end-to-end acceptance over the real server

**Tier:** standard

**Files:**
- Create: `cmd/fuse/loop_serve_net_cancel_test.go`

**Step 1: Write the tests** (they should pass once Tasks 1-8 are in; any failure is a wiring defect to fix in this task, most likely in `cmd/fuse`). Model the harness on `TestLoopServeNetEnforcesTenantLimitsOverH2C` (`cmd/fuse/loop_serve_net_limits_test.go`) and `TestObservabilityAcceptanceHermetic`: write `~/.fuse/config.yml` under a temp `HOME`, override `netListen` and `serveNetContext`, run `run([]string{"loop-serve-net", "--addr", ...}, &logs, &bytes.Buffer{})` in a goroutine, and restore the globals in `t.Cleanup`. Set `LLM_GATEWAY_URL`/`LLM_GATEWAY_KEY` to the scripted gateway so the test is hermetic on a developer machine.

Holding gateway (`httptest.NewServer`, closed via `t.Cleanup`): it reads the body; if it contains `HOLD` it sets an atomic `holding` flag, blocks on `<-r.Context().Done()`, then sets an atomic `sawCancel` flag and returns; otherwise it answers `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`.

Config: `loop_server.auth` with `owner-tok` (tenant `acme`, subject `owner`), `peer-tok` (`acme`, `peer`), `other-tok` (`globex`, `other`); `observability.metrics` enabled at `/metrics` with `access: public`; `observability.logging` enabled to stdout at debug; `cardinality.tenant.catalog: [acme, globex]` with a budget of 3 (copy the rest of the cardinality block from the observability acceptance test). The limits test gets its own config that adds `loop_server.limits.default.max_live_loops: 1`.

- `TestCancelLoopOverTheWire` (criteria 12 and 14): readiness-retry a first call as the limits test does. `owner` StartLoop `{task: "HOLD please"}`. Open Observe from seq 0 and read until a `model.call.start` event arrives (bounded 5s). `CancelLoop{loop_id, reason: "operator stop"}` returns `CANCEL_OUTCOME_CANCELLED`, `was_live == true`, `terminal_seq > 0`, and a `Fuse-Trace-Id` header when tracing is on (skip that assertion if the harness has no tracer). Keep reading the SAME Observe stream until `loop.cancelled` arrives (bounded 5s): its payload reason is `operator stop`, its seq equals `terminal_seq`, and no event with a higher seq arrives within 200ms. `sawCancel` is true (`eventually`). `Send{input:"hi"}` is `FailedPrecondition` with a message containing `cancelled` and not `finished`. A second `CancelLoop` returns `ALREADY_CANCELLED` with reason `operator stop`. GET `/metrics` contains the line `fuse_loops_cancelled_total{tenant_id="acme",was_live="true"} 1` (poll up to 2s: projection is asynchronous). The captured logs contain a JSON line with `"event_name":"loop.cancelled"`, `"operation":"loop"` and `"outcome":"canceled"` (check the exact keys in `internal/observe/logging/logger.go`) and do not contain `operator stop` if the logger is payload-free (assert whichever holds after reading the logger; the reason must never appear in a metric label).
- `TestCancelLoopTenantIsolationOverTheWire` (criterion 3): owner starts a `HOLD` loop. `other-tok` CancelLoop is `NotFound`; `peer-tok` is `PermissionDenied`; `owner-tok` with wire `tenant: "globex"` is `PermissionDenied`. The loop is still running afterwards (`holding` true, no `loop.cancelled` in an Observe replay). Then owner cancels it (cleanup, and proves the loop was intact).
- `TestCancelLoopFreesLiveLoopSlot` (criterion 13): with `max_live_loops: 1`: StartLoop `HOLD` succeeds; a second StartLoop is `ResourceExhausted` (proves the cap bites); CancelLoop is `CANCELLED`; a StartLoop issued immediately after (no sleep, no retry) succeeds.

**Step 2:** `go test ./cmd/fuse -run 'TestCancelLoop' -count=1 -v`.

**Step 3:** fix any wiring gap found (expected none beyond Task 8's projecting store). If the handler answers `Unimplemented`, something in `cmd/fuse` hides the runtime's `Canceller`; fix the composition, not the test.

**Step 4:** `go test ./cmd/fuse -count=1` and `go test -race ./cmd/fuse -run 'TestCancelLoop|TestLoopServeNet|TestAuth' -count=1` pass.

**Step 5: Commit** `test(fuse): CancelLoop end to end over loop-serve-net`.

---

## Task 11: TS SDK - `client.cancel`, `KIND_LOOP_CANCELLED`, `isCompletion`

**Tier:** economy

**Files:**
- Modify: `sdk/ts/src/index.ts`
- Create: `sdk/ts/test/cancel.test.ts`
- Modify: `sdk/ts/README.md`

**Step 1: Failing tests** in `cancel.test.ts`, using `createRouterTransport` exactly as `steer.test.ts` does: the router's `cancelLoop(req)` records `{loopId, reason, tenant}` and returns `{outcome: CancelOutcome.CANCELLED, wasLive: true, terminalSeq: 9n, reason: req.reason}` (import the generated `CancelOutcome` enum from `loop_pb.js`; check the generated name). Tests:
- `client.cancel("loop-1", "operator stop")` sends `loopId`, `reason`, and the credentials tenant, and resolves to `{ outcome: "cancelled", wasLive: true, terminalSeq: 9n, reason: "operator stop" }`.
- `client.cancel("loop-1")` sends `reason: ""`.
- `ALREADY_CANCELLED` maps to `"already_cancelled"`, `CANCELLING` to `"cancelling"`, and `UNSPECIFIED` throws (a server bug, not an outcome).
- A router error with `Code.PermissionDenied` rejects with that `ConnectError` unchanged.
- `isCompletion({kind: "loop.cancelled", ...})` is true; `isCompletion` of `loop.parked` stays true; of `model.call.end` false. `KIND_LOOP_CANCELLED === "loop.cancelled"`.

**Step 2:** `make sdk-ts-test` fails.

**Step 3: Implement** in `index.ts`:
```ts
/** The terminal event kind of a cancelled loop. */
export const KIND_LOOP_CANCELLED = "loop.cancelled";

/** What a {@link Client.cancel} did. */
export type CancelOutcome = "cancelled" | "already_cancelled" | "cancelling";

/** The result of {@link Client.cancel}. */
export interface CancelResult {
  outcome: CancelOutcome;
  /** True when the cancel interrupted a running or parked loop. */
  wasLive: boolean;
  /** Seq of the loop.cancelled event; 0n when the outcome is "cancelling". */
  terminalSeq: bigint;
  /** The recorded reason (the first cancel's on "already_cancelled"). */
  reason: string;
}
```
`isCompletion` returns true for `KIND_LOOP_PARKED` or `KIND_LOOP_CANCELLED` (update its doc comment: a cancelled loop's stream ends there). Add to the `Client` interface:
```ts
  /**
   * cancel stops a loop and seals it: its work is interrupted, loop.cancelled is the
   * last event of its stream, and it never accepts another send. Idempotent. Needs the
   * loop:cancel scope (or loop:admin).
   */
  cancel(loopId: string, reason?: string): Promise<CancelResult>;
```
and implement it over `wire.cancelLoop({ loopId, reason: reason ?? "", tenant })` with a switch mapping the generated enum. `TERMINAL_CODES` is unchanged (`FailedPrecondition` already stops the observe reconnect). README: a `client.cancel(loopId, "reason")` line in "Drive a loop" and a sentence in "Reconnect / completion" that `isCompletion` is also true for `loop.cancelled`, after which the loop never runs again.

**Step 4:** `make sdk-ts-test` passes.

**Step 5: Commit** `feat(sdk-ts): client.cancel and loop.cancelled completion`.

---

## Task 12: Documentation

**Tier:** economy

**Files:**
- Modify: `README.md` (Scopes table near line 600; the RPC list for the networked server: `grep -n 'GetCapabilities\|StartLoop' README.md` to find it)
- Modify: `deploy/charts/fuse/values.yaml` (the `scopes:` comment near line 138)
- Modify: `docs/observability.md`

**Steps:**
1. README Scopes table: add the row `| \`CancelLoop\` | \`loop:cancel\` (or \`loop:admin\`) |` after `Observe`. Under the bullets: `loop:cancel` is implied by no other scope (a start-only token cannot stop loops); an unrestricted token has it. In the RPC list, describe CancelLoop in two or three sentences: stops a loop the caller owns, releases its resources, `loop.cancelled` is the last event, Send afterwards is `FailedPrecondition` "loop cancelled", idempotent, outcomes `CANCELLED` / `ALREADY_CANCELLED` / `CANCELLING`. Say "callers", never name a host application.
2. `values.yaml`: extend the comment to list the vocabulary, e.g. `#     scopes: [loop:observe]           # optional; omitted = unrestricted. loop:start loop:send loop:observe loop:cancel loop:admin capabilities:read secrets:inline (see README Scopes)`. Keep the column alignment of the surrounding lines.
3. `docs/observability.md`: add a short "Loop cancellation" section listing `fuse_loops_cancelled_total{tenant_id, was_live}` (counter, projected from `loop.cancelled`; `was_live` is `true|false`; the reason is never a label), and add `cancelled = 2*T` to the per-replica capacity formula.
4. Run `make observability-validate` and `go test ./cmd/fuse -run 'Help|Readme|Doc' -count=1` (any doc-pinning tests) to confirm nothing pinned broke.

**Commit:** `docs: CancelLoop, loop:cancel scope, fuse_loops_cancelled_total`.

---

## Task 13: ADR (parent task, not a build worker)

**Tier:** parent (run by `docket-implement-next` through the `docket-adr` agent; a build worker skips this task)

Record one ADR, status Accepted. Suggested title: "Loop cancellation is a terminal stream event plus an in-memory seal". Content:
- Decision: a cancelled loop is marked by a terminal `loop.cancelled` event (the highest seq of its stream) and, in the owning process, an in-memory seal. The registry only flips `live`. `Resume` refuses a loop by replaying its stream: a `loop.cancelled`, or a `loop.cancel.request` with no `loop.cancelled` after it (an unanswered relayed request is as good as a cancel, and the refusing replica seals it). Cross-replica cancel relays through the durable stream like `steer.request`.
- Rationale: the stream stays the single source of truth (0054 D1/D5); no schema migration; both registry backends and every registry fake are untouched.
- Alternatives considered: a `cancelled` column / `MarkCancelled` on `LoopRegistry` (rejected: migration for pgstore and fsstore, every fake grows a method, and the registry would duplicate a fact the stream already holds); terminal events for idle reaps and drains (rejected: those loops are resumable by design); a direct replica-to-replica RPC (rejected: replicas do not know each other's addresses; the relay pattern already exists).
- Link change 0117. Record it before the PR is opened so the PR body can reference the ADR id.

---

## Task 14: Final verification

**Tier:** economy

1. `make proto` then `git status --porcelain`: no diff (generated files committed and current).
2. `go build ./...`
3. `make test` (the full gate, includes `observability-validate`).
4. `go test -race ./internal/runtime ./internal/loopconnect ./internal/observe/... ./cmd/fuse -count=1`
5. `make sdk-ts-test`
6. `grep -rn 'Canceller' --include='*.go' cmd internal | grep -v _test` shows the handler's type assertion and the runtime's implementation (the capability is reachable in the binary, not only in tests).
7. Scan the diff for host-application names in comments, docs, commit messages and test names; there must be none ("callers" only).

No commit unless a step required a fix (then `fix: ...` describing it).

---

## Acceptance coverage map

| Criterion | Where |
| --- | --- |
| 1 proto + `make proto` + build/test | Task 3, Task 14 |
| 2 scopes | Task 2, Task 7 `TestCancelLoopScopes` |
| 3 cross-tenant | Task 7 `TestCancelLoopTenantIsolation`, Task 10 `TestCancelLoopTenantIsolationOverTheWire` |
| 4 validation, Unimplemented | Task 7 `TestCancelLoopValidation`, `TestCancelLoopUnimplemented` |
| 5 cancel during model call | Task 4 `TestCancelOneShotDuringModelCall` |
| 6 cancel during tool, teardown | Task 4 `TestCancelDuringToolCall` |
| 7 child unwinds, slots freed | Task 5 `TestCancelWhileChildRunning` |
| 8 parked interactive loop | Task 4 `TestCancelParkedInteractiveLoop` |
| 9 idempotency, finished, reaped | Task 4 `TestCancelIdempotent`, `TestCancelFinishedLoopSeals`, `TestCancelIdleReapedLoopSeals`; Task 6 `TestCancelNotLiveOnColdReplica` |
| 10 cross-replica | Task 6 `TestCancelOnNonOwnerRelays`, `TestCancelRelayPendingAppendsOnce`, `TestResumeRefusesUnansweredCancelRequest` |
| 11 drain/reap unchanged | Task 4 `TestDrainAndReapEmitNoTerminal` plus the existing drain tests |
| 12 over the network | Task 10 `TestCancelLoopOverTheWire` |
| 13 frees the live-loop slot | Task 10 `TestCancelLoopFreesLiveLoopSlot` |
| 14 metric and log projection | Task 8 unit tests, Task 10 `/metrics` and log assertions |
| 15 TS SDK | Task 11 |
| 16 docs and ADR | Task 12, Task 13 |
