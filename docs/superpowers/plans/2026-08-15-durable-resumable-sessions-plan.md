# Durable, resumable sessions — build plan

Change [#0054](../../changes/active/0054-durable-resumable-sessions.md) ·
Spec: [2026-08-15-durable-resumable-sessions-design.md](2026-08-15-durable-resumable-sessions-design.md
"spec lives on metadata branch") · Base: `origin/main` @ `e6e637f`

TDD, one commit per task. Each task writes its focused test first.

## Task 1 — `KindUserInput` (event stream completeness)

Reconcile finding: the durable stream has **no** event for user/human input — the
initial `cfg.Task` and each `Send` payload are appended to the in-memory transcript but
never emitted, so a fold from events alone would drop every user turn. Add a durable
`KindUserInput` (`"user.input"`) + `UserInputPayload{Turn, Content}`, pinned in
`event_test.go`. Emit it in `Agent.Run` for the seed's user turns and at each human
`Poll`. Add `Agent.SetSeeded` so a resumed loop (whose seed IS a reconstruction) does not
re-emit turns already in the stream. Update `TestEmitPlainTurn`'s expected sequence.

## Task 2 — `events → []model.Message` fold (D5)

`internal/runtime/reconstruct.go`: single forward pass over Seq-ordered events.
`KindUserInput`→user; `KindModelCallEnd`(+ same-turn `KindToolCall` args backfilled by
call ID)→assistant with tool calls; `KindToolResult`→tool; everything else skipped.
Round-trip test drives a real multi-turn interactive loop incl. a tool call, captures the
live transcript, folds the loop's OWN durable events, asserts **byte-equal**.

## Task 3 — session ctx severed from request ctx + idle-TTL reaper (D2)

Interactive loops run under `context.WithCancel(context.WithoutCancel(loopCtx))` — keeps
trace/identity values, drops request cancellation, so a disconnect no longer unwinds the
park. `durableSink.ctx` and the lease renewer follow the session ctx. Per-loop idle-TTL
reaper (`Deps.IdleTTL`, default 30m) cancels the session on no Send/Observe within the
window; `Send`/`Observe` `touch()` to reset. Tests: disconnect-survives, idle-reap
(no goroutine/store leak). Updates the pre-existing park/resume test to the new contract.

## Task 4 — runtime `Resume` seam (D4)

`Resume(ctx, tenant, loopID)` on `runtime.Runtime`. Tenant-scoped (ADR-0034;
cross-tenant/unknown → `ErrLoopNotFound`). Live-here → no-op returning the handle.
Finished/evicted → replay the durable stream, fold (D5), re-launch a re-parked loop under
the SAME loop_id seeded with the transcript (`SetSeeded`), re-mark live. Live-elsewhere →
not-found. Factor `StartLoop`'s construction into a shared `launchLoop`; add
`agent.NewAgentTreeWithRootID` so a resumed tree keeps the original loop_id. Tests: cold
cross-instance rehydrate, live no-op, unknown not-found.

## Task 5 — loopconnect resume routing (D3) + acceptance

`Handler.Send`: on `ErrLoopFinished`, call `Resume` and retry once; unresumable still maps
to `CodeFailedPrecondition`. `Observe` already replays a finished loop's history (Attach),
so no wire change. Handler unit acceptance + real-server cold cross-instance acceptance in
`cmd/fuse` (start interactive on A → park → reap → cold `Send` on B → transparent resume →
transcript replays with both parks).

## Task 6 — full-suite gate + PR

`make test` green; add `Resume` no-op to the remaining `Runtime` test doubles; open the PR.
