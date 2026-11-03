<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0117 — Cancel RPC on the networked loop server](../../changes/archive/2026-11-03-0117-cancel-rpc-on-the-networked-loop-server.md)**
<!-- docket:backlink:end -->

# Cancel RPC on the networked loop server

- Type: feat
- Priority: high

## Why

`proto/fuse/loop/v1/loop.proto` exposes StartLoop, Send, Observe and GetCapabilities. Nothing lets a caller stop a loop it started. A one-shot loop runs to completion under the server's lifetime context; an interactive loop parks until its idle TTL (default 30m, `internal/runtime/inproc.go` `defaultIdleTTL`) reaps it. Until then the loop holds a `max_live_loops` slot (change 0103 counts live registry records), its per-loop sandbox pool and MCP manager (released only by `LoopTeardown` at run completion), and whatever model call or tool is in flight keeps spending tokens and wall clock. Callers that track live loops have no way to release one, and nobody short of the operator draining the whole server can interrupt a loop that is stuck.

Today only interactive loops even have a per-loop cancel (`loop.cancel`, the session cancel the idle reaper and `Shutdown` use); a one-shot loop has none. And because `Send` on a finished loop transparently `Resume`s it (`internal/loopconnect/handler.go`, the `ErrLoopFinished` branch), "stopping" a loop by any back-door means would be undone by the next message.

## What changes

### 1. Wire: `CancelLoop` on `LoopService`

Add to `proto/fuse/loop/v1/loop.proto` (regenerate Go + TS stubs with `make proto` and commit them):

```proto
// CancelLoop stops a loop the caller owns and seals it: its in-flight work is
// cancelled, its resources are released, loop.cancelled is appended as the last
// event of its stream, and the loop never accepts another Send or resume.
// Idempotent. Same ownership rule as Send; requires loop:cancel (or loop:admin).
rpc CancelLoop(CancelLoopRequest) returns (CancelLoopResponse);

message CancelLoopRequest {
  string loop_id = 1;
  string tenant = 2;   // same pass-through rule as SendRequest.tenant
  // reason is free text the caller records with the cancel (at most 256 bytes,
  // no control characters; InvalidArgument otherwise). Optional.
  string reason = 3;
}

enum CancelOutcome {
  CANCEL_OUTCOME_UNSPECIFIED = 0;
  // This call sealed the loop; was_live says whether it stopped running work.
  CANCEL_OUTCOME_CANCELLED = 1;
  // An earlier cancel already sealed it; reason and terminal_seq are that cancel's.
  CANCEL_OUTCOME_ALREADY_CANCELLED = 2;
  // The loop is owned by another replica; the request is recorded in its stream
  // and the owner will emit loop.cancelled. terminal_seq is 0.
  CANCEL_OUTCOME_CANCELLING = 3;
}

message CancelLoopResponse {
  CancelOutcome outcome = 1;
  // true when this cancel interrupted a running or parked loop; false when it
  // sealed a loop that had already finished or been idle-reaped.
  bool was_live = 2;
  // seq of the loop.cancelled event; 0 when outcome is CANCELLING or the store
  // cannot report it.
  uint64 terminal_seq = 3;
  // The recorded reason (the original one on ALREADY_CANCELLED).
  string reason = 4;
}
```

Error codes, checked in this order (same shape as Send):
- `PermissionDenied`: principal lacks `loop:cancel` (and not `loop:admin`); wire tenant mismatches the principal's tenant; loop owned by another subject in the same tenant without `loop:admin`.
- `NotFound`: unknown loop, or a loop in another tenant (tenant-scoped resolve, ADR-0034; never a leak).
- `InvalidArgument`: empty `loop_id`; `reason` over 256 bytes or containing control characters.
- `Unimplemented`: the runtime does not implement the cancel capability (see §3).
- `Internal`: anything else.

### 2. Auth: a new `loop:cancel` scope

- `internal/loopauth/scopes.go`: add `ScopeLoopCancel = "loop:cancel"`. CancelLoop requires it; `loop:admin` satisfies it and lifts the owner check within the tenant exactly as for Send/Observe. An unrestricted token (no scopes) has it. `loop:start`, `loop:send` and `loop:observe` do NOT imply it (a job submitter's start-only token must not be able to kill loops; a watchdog may hold cancel alone).
- Document it in `README.md` (Scopes table and the RPC list), the `scopes:` comment in `deploy/charts/fuse/values.yaml`, and the `Handler` doc comment in `internal/loopconnect/handler.go`.
- No new rate limit: cancel is idempotent and cheap. The only write it can cause on a loop it does not own locally is the relay request in §3.4, and the edge appends at most one such request per loop per process (a process-local pending set keyed by `StreamKey`); further calls while one is pending return `CANCELLING` without appending.

### 3. Runtime: an optional `Canceller` capability (ADR-0047 pattern)

In `internal/runtime/runtime.go`:

```go
// Canceller is an optional Runtime capability: stop a loop and seal it.
type Canceller interface {
    Cancel(ctx context.Context, tenant event.TenantID, loopID, reason, requestedBy string) (CancelResult, error)
}
type CancelResult struct {
    Outcome     CancelOutcome // Cancelled | AlreadyCancelled | Cancelling
    WasLive     bool
    TerminalSeq event.Seq
    Reason      string
}
var ErrLoopCancelled = errors.New("runtime: loop cancelled") // distinct from ErrLoopFinished
```

The Connect handler type-asserts for it and answers `Unimplemented` otherwise, so `netFakeRuntime` and the other fakes keep compiling. `Cancel` returns `ErrLoopNotFound` for unknown or wrong-tenant loops.

#### 3.1 Every loop gets a per-loop cancel with a cause

In `launchLoop`: derive the run context for EVERY loop (one-shot and interactive) from a `context.WithCancelCause` held on the `loop` struct in a new field (say `abort context.CancelCauseFunc`), parented on `sessionCtx` (interactive) or `loopCtx` (one-shot). `Cancel` fires it with cause `ErrLoopCancelled`. Keep the existing `loop.cancel` (session cancel) and `Shutdown` exactly as they are: `Shutdown` must not start cancelling one-shot loops it does not cancel today, and the idle reaper must keep using the session cancel with no cause, so the completion goroutine can tell a caller cancel (`context.Cause(runCtx) == ErrLoopCancelled`) from a reap or drain.

#### 3.2 Local path (loop in this process's cache under the requested tenant)

1. Under `r.mu`, if `lp.cancelled` is already set return `AlreadyCancelled` with the recorded reason and seq. Otherwise record `lp.cancelled = {reason, requestedBy}` and fire `lp.abort(ErrLoopCancelled)`. If the run had already finished (`handle.done` closed) before this call, skip to the "seal" step below with `was_live=false`.
2. The run unwinds through context cancellation, nothing else:
   - in-flight model call: `interruptScope` derives from the run ctx, so `model.Complete` returns with a cancelled ctx; the agent emits `model.call.end`/`error` as it does today and returns `ctx.Err()`;
   - running tools: `executeToolBounded`'s ctx is derived from the run ctx; the sandbox runs commands with `exec.CommandContext` so the process is killed; the tool result is recorded as cancelled, as today;
   - child agents and their pools: `Spawner.spawnLocal` derives `childCtx` from the tool ctx, so every descendant unwinds, emits `spawn.done` with its error, and releases its scheduler slot; pipeline/workflow spawns are descendants too;
   - pending approvals: the networked binding runs `AlwaysApprove`, so none exist; the `ApprovalFunc` contract already takes ctx, nothing to add;
   - a parked interactive loop: `humanInjector.Wait(ctx)` returns, `Run` returns cleanly, the idle reaper is stopped by the completion path.
3. Completion goroutine (`launchLoop`'s `go func`): after `a.Run` returns and `turns.teardown`/`end` have run, and BEFORE `SetLive(false)`, store Close and `LoopTeardown`, if `lp.cancelled` is set append the terminal event:

```go
KindLoopCancelled Kind = "loop.cancelled"
type LoopCancelledPayload struct {
    Reason      string `json:"reason,omitempty"`
    RequestedBy string `json:"requested_by,omitempty"` // principal subject, never a token (ADR-0046)
    WasLive     bool   `json:"was_live"`
    RequestSeq  uint64 `json:"request_seq,omitempty"`  // seq of the loop.cancel.request it answers; 0 for a local cancel
}
```

   Ordering guarantee: `loop.cancelled` has the highest seq of every event this loop ever appends. Every event the unwinding produced (`error`, `tool.result`, `spawn.done`, `loop.parked`) precedes it; the relay pump and mint-audit binding are stopped/unbound before or on the same goroutine, so nothing follows it. Observers read it as the terminal signal.
4. `SetLive(false)` lands durably (as today), then `LoopTeardown` releases the per-loop sandbox pool and MCP manager (as today).
5. `Cancel` waits for `handle.done` bounded by the caller's ctx and a cap (`Deps.CancelWait`, default 10s), then returns `Cancelled{WasLive:true, TerminalSeq, Reason}`. If the wait expires it returns `Cancelling` (the unwind is still running; the terminal event follows). `CANCELLED` therefore means the registry already shows the loop not live: a tenant at `max_live_loops` can StartLoop immediately after.
6. Sealing: after cancel, `liveLoop` returns `ErrLoopCancelled` (not `ErrLoopFinished`) for a cached loop with `lp.cancelled` set, so the handler's Send never attempts the resume retry. `Resume` returns `ErrLoopCancelled` for it too. `Observe`/`Attach` keep working (read-only).

#### 3.3 Not in the cache (finished here, or cold instance)

Resolve tenant-scoped via the registry (`resolveDurableForDelivery` semantics: a live record with an expired lease is reaped to not-live, never re-owned). Unknown or wrong tenant: `ErrLoopNotFound`. Then:

- Record NOT live: replay the stream (`DurableStore.Replay(ctx, key, 0)`). If it already contains `loop.cancelled`: `AlreadyCancelled` with that event's reason and seq. Otherwise append `loop.cancelled{was_live:false, request_seq: seq of any unanswered loop.cancel.request}` and return `Cancelled{WasLive:false}`. This seals a finished one-shot loop and a parked-then-reaped interactive loop alike, which is the uniform rule a caller counting loops needs: after CANCELLED, that id never runs again.
- Record live with a fresh lease, owned elsewhere: §3.4.

#### 3.4 Cross-replica relay

Append `loop.cancel.request` to the loop's stream (new kind, payload `{reason, requested_by}`), the same mechanism `steer.request` uses (`internal/runtime/relay.go`). Extend `startRelayPump` to treat a `loop.cancel.request` exactly like a local `Cancel` (so the owner's completion path appends `loop.cancelled` with `request_seq` set); the pump ignores further requests once one is being honoured. The requesting runtime then subscribes to the stream (`DurableStore.Subscribe` + replay from the request seq) and waits, bounded by ctx and `CancelWait`, for `loop.cancelled`; it returns `Cancelled{WasLive:true, TerminalSeq}` if it arrives, else `Cancelling`.

An unanswered `loop.cancel.request` is as good as a cancel: `Resume` (which already replays the whole stream) must refuse with `ErrLoopCancelled` when the replay holds a `loop.cancelled` OR a `loop.cancel.request` with no `loop.cancelled` after it, sealing the latter case by appending `loop.cancelled{was_live:false, request_seq}` first. This covers an owner that died between receiving the request and acting on it. `reconstructMessages`, `harnessFromEvents`, `consumedTurns` and the relay's `pendingRequests` must ignore both new kinds.

#### 3.5 Orchestrators

Cancelling a loop does not cancel a loop that orchestrates it, and cancelling an orchestrator does not cancel its target. An orchestrator whose target is cancelled gets `ErrLoopCancelled` from `steer_agent` and sees `loop.cancelled` through `loop_event`; no new orchestrator tool.

### 4. Connect edge (`internal/loopconnect`)

`Handler.CancelLoop`: open a `fuse.api.request` span named `cancel_loop` with `Fuse-Trace-Id` on the response header like Send; `requireScope(ScopeLoopCancel, ScopeLoopAdmin)`; `effectiveTenant`; `authorizeLoop`; validate `reason`; type-assert `runtime.Canceller` (else `Unimplemented`); call `Cancel(ctx, tenant, loopID, reason, p.Subject)`; map `ErrLoopNotFound` to NotFound, `ErrLoopCancelled` to FailedPrecondition. `Handler.Send`: map `ErrLoopCancelled` to `FailedPrecondition` with the message `loop cancelled` (distinct from `loop finished`) and never enter the resume retry for it. This composes with change 0108 (which only changes how a Resume error is surfaced).

### 5. Observability

- Metric: `fuse_loops_cancelled_total{tenant_id, was_live}` (counter), registered in the Prometheus recorder catalog (`internal/observe/prometheus/recorder.go`) and incremented from `Recorder.Project` off a `loop.cancelled` record, the way `fuse_permission_decisions_total` is. `was_live` is `"true"|"false"`. The caller's `reason` is free text and must NOT be a label (cardinality; same rule that keeps `container_id` out). `tenant_id` goes through `metricspolicy` as every other tenant label. Add the metric to the catalog in `docs/observability.md`. No dashboard panel is required (the validator is exhaustive over panels, not over metrics).
- Projection: `observe.ProjectEvent`/`classify` maps `KindLoopCancelled` to `OperationLoop`, `OutcomeCanceled`, no error category, so the committed-event log sink records it with tenant, loop and trace ids like every other event (ADR-0038). `loop.cancel.request` classifies like `steer.request`.
- Audit: the stream is the audit record. `loop.cancelled.requested_by` is the principal subject that called CancelLoop (or that relayed it), never a token or digest, matching `loop.principal`.

### 6. TS SDK (`sdk/ts`)

- `Client.cancel(loopId: string, reason?: string): Promise<CancelResult>` wrapping the generated `cancelLoop`.
- `KIND_LOOP_CANCELLED = "loop.cancelled"`; `isCompletion` returns true for it as it does for `loop.parked`.
- README entry. Existing observe reconnect/terminal semantics (ADR-0037) are unchanged.

### 7. ADR

Record one ADR via `docket-adr`: cancellation is a terminal stream event plus an in-memory seal, not a registry column; the registry only flips `live`; resume refuses by replay; an unanswered relayed request is as good as a cancel. Rationale: the stream stays the single source of truth (0054 D1/D5), no schema migration, and both registry backends and every registry fake are untouched.

## Out of scope

- Cancelling a single child, tool call or turn (Send with `interrupt` already covers in-turn steering).
- An orchestrator `cancel_agent` tool; `loop.cancel` on the stdio JSON-RPC binding (`internal/loopserver`); a CLI flag.
- Emitting `loop.cancelled` (or any terminal event) for idle reaps and graceful drains: those loops remain resumable by design and are not cancelled.
- A `cancelled` column or `MarkCancelled` method on `LoopRegistry`; listing cancelled loops; retention or purging of cancelled streams.
- A bounded reason vocabulary as a metric label; dashboards and alert rules for `fuse_loops_cancelled_total`.
- Changing idle-TTL defaults, drain behaviour, or `Shutdown`.
- Advertising cancel support in GetCapabilities (clients detect it by `Unimplemented`).
- Anything specific to a particular host application; the API speaks only of callers.

## Acceptance criteria

Wire and auth
1. `proto/fuse/loop/v1/loop.proto` has `CancelLoop` with the messages and enum above; `make proto` output is committed and `go build ./... && make test` pass.
2. A token with `loop:cancel` can cancel its own loop; a token with only `loop:send loop:observe loop:start` gets `PermissionDenied` naming `loop:cancel`; `loop:admin` cancels another subject's loop in the same tenant; an unrestricted token cancels (`internal/loopconnect` handler tests, mirroring `TestSendCrossOwnerSameTenantDenied` et al.).
3. Cross-tenant: with `twoTenantVerifier`, `other-tok` cancelling `owner-tok`'s loop gets `NotFound`; `peer-tok` gets `PermissionDenied`; a mismatching wire `tenant` is `PermissionDenied`. Proven both at the handler and over the real server (criterion 12).
4. A 257-byte or control-character `reason` is `InvalidArgument`; an empty `loop_id` is `InvalidArgument`; a runtime without `Canceller` yields `Unimplemented` (test with `netFakeRuntime`).

Runtime semantics (unit tests in `internal/runtime`, using `scriptedCompleter`-style fakes that block on ctx)
5. Cancelling a one-shot loop blocked in `model.Complete`: the completer's ctx is cancelled, `Run` returns `context.Canceled`, the stream ends with `loop.cancelled{was_live:true, reason}` as its highest-seq event (after the agent's `error` event), the registry record is not live before `Cancel` returns `Cancelled`, and `TerminalSeq` matches the event.
6. Cancelling during a tool call that blocks on ctx: the `tool.result` is recorded as cancelled and precedes `loop.cancelled`; `ReleaseSandboxes`/`LoopTeardown` ran (observe via the `Deps.LoopTeardown` hook).
7. Cancelling while a spawned child is running: the child's `spawn.done` (with a cancellation error) precedes `loop.cancelled`; the scheduler's slot count returns to zero.
8. Cancelling a parked interactive loop: `loop.parked` then `loop.cancelled`; the idle reaper does not fire afterwards; `Send` returns `ErrLoopCancelled`; `Resume` returns `ErrLoopCancelled` and launches nothing; `Observe`/`Attach` still serve history.
9. Idempotency: a second `Cancel` returns `AlreadyCancelled` with the first reason and seq and appends nothing. `Cancel` on a loop that finished on its own returns `Cancelled{WasLive:false}` and appends `loop.cancelled{was_live:false}`; a later `Send` is `ErrLoopCancelled`, not a resume. `Cancel` on an idle-reaped interactive loop seals it the same way.
10. Cross-replica (two runtimes over one fsstore durable store, as in `loop_serve_net_twoinstance_test.go`): `Cancel` on the non-owner appends `loop.cancel.request`, the owner's relay pump stops the loop, `loop.cancelled{request_seq}` is the last event, and the non-owner returns `Cancelled{WasLive:true}` within `CancelWait`; with the owner stopped (lease expired), `Resume` on the other replica refuses with `ErrLoopCancelled` and seals the unanswered request. A second concurrent request from the same process appends nothing.
11. `Shutdown` and the idle reaper behave exactly as before: no `loop.cancelled` is emitted by a drain or a reap, and `Shutdown` still does not cancel one-shot loops (existing drain tests pass unchanged).

Integration (`cmd/fuse`, real `serveNet` + real runtime + fsstore durable store; a scripted HTTP gateway only, no Anthropic traffic)
12. Over the network: a gateway that holds its response until the request ctx ends; `StartLoop`, `Observe` until `model.call.start`, `CancelLoop{reason:"operator stop"}` returns `CANCELLED`, `was_live=true`; the Observe stream then delivers `loop.cancelled` with that reason as its last event; the gateway's handler saw its request ctx cancelled; `Send` afterwards is `FailedPrecondition` whose message contains `cancelled`, not `finished`. The same test file covers criterion 3 over the wire with `twoTenantVerifier`.
13. With `loop_server.limits.default.max_live_loops: 1`: StartLoop, CancelLoop → CANCELLED, then a second StartLoop succeeds immediately (no `RESOURCE_EXHAUSTED`).
14. `/metrics` shows `fuse_loops_cancelled_total{tenant_id="acme",was_live="true"} 1` after criterion 12 (recorder projection test plus the end-to-end scrape via the existing observability test harness), and the projected log record for `loop.cancelled` carries `operation=loop outcome=canceled`.

SDK and docs
15. `sdk/ts`: `client.cancel(loopId, reason)` returns the outcome; `isCompletion` is true for `loop.cancelled`; `make sdk-ts-test` passes.
16. `README.md` Scopes table lists `CancelLoop | loop:cancel (or loop:admin)`; `values.yaml` scope comment, `docs/observability.md` metric catalog, and the proto comments are updated; one ADR is recorded per §7.
