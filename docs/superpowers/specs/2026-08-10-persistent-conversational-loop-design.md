<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0053 — Persistent conversational loop — interactive mode so one loop_id carries a multi-turn chat](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0053-persistent-conversational-loop.md)**
<!-- docket:backlink:end -->

# Spec 0053 — Persistent conversational loop: interactive mode

## Problem

A fuse loop is one task → completion. In `internal/agent/loop.go`, the turn loop returns
at its terminal path — a model response with **no tool calls**:

```go
if len(resp.ToolCalls) == 0 {
    a.emit(event.KindTurnEnd, turn, event.TurnEndPayload{Turn: turn})
    return messages, nil          // ← run ends here
}
```

When `Run` returns, the runtime's run goroutine (`internal/runtime/inproc.go`) marks the
loop finished in the durable registry (`SetLive(false)`) and closes the loop's event store
(which closes live observe channels). After that, `runtime.Send` on that loop returns
`ErrLoopFinished` — its human-injector no longer drains, so enqueuing would strand the
message.

That contract is correct for a one-shot job and is what every binding (CLI, stdio
loop-server, and the 0048 networked binding) has always assumed. It is **wrong for a
chat**. Over the 0048 WebSocket surface, a conversational client:

1. `loop.start` "find me a Tulum rental" → loop runs, answers, **finishes**.
2. `loop.send` "what about Aspen?" on the same `loop_id` → **`ErrLoopFinished`**.

The only workaround is a fresh `loop.start` every turn with the prior conversation
re-serialized into the task prompt. That means N `loop_id`s, N event streams, the client
re-shipping history each turn, and no server-side session of record — the opposite of
0048's "attach to your *running* loop from your phone" goal.

A second, subtler symptom of the same gap: there is **no deterministic completion signal**.
A one-shot client knew a turn was done because the run ended and the stream closed. A
conversational client, with the loop persisting, has to *infer* "answer ready" from the
event shape ("a no-tool `model.call.end`, then `turn.end`"). That heuristic desyncs — e.g.
a turn that emits content *and* a trailing tool call — and the UI hangs waiting for a
completion that, by its own bookkeeping, already passed. (Observed live while building the
demo.)

Both are the same missing primitive: **a loop that persists across turns, with a clean
"your turn" signal.**

## Non-goals (deferred, do not design here)

- **Session ownership / auth / tenant identity** — change 0049. `tenant_id` flows
  present-but-unenforced as in 0048.
- **External session wire envelope / SDK session ergonomics** — change 0050.
- **Cross-instance resume of a finished/evicted session** from durable history (transcript
  rehydration + registry liveness). A parked loop's transcript is in owning-process memory.
- **Idle/session timeouts, abandoned-park backpressure, max concurrent sessions** —
  operational policy.
- **`model.delta` streaming** for the chat surface.

## Design

Five small pieces, all over the **existing** Runtime seam — no new transport, no new handle
type, no wire-envelope change. Everything keys off the loop's existing human-message bus
(ADR-0022) and turn boundary.

### 1. Park instead of finish (`internal/agent`)

`Agent` gains an `interactive bool` field (default false) with `SetInteractive(bool)`. At
the terminal path, when interactive **and** a human injector is wired:

```go
if len(resp.ToolCalls) == 0 {
    a.emit(event.KindTurnEnd, turn, event.TurnEndPayload{Turn: turn})
    if a.interactive && a.humanInjector != nil {
        a.emit(event.KindLoopParked, turn, event.LoopParkedPayload{Turn: turn, Content: resp.Content})
        if werr := a.humanInjector.Wait(ctx); werr == nil {
            continue            // message pending → next turn's Poll injects it
        }
        // errNoBus or ctx cancellation → fall through to the normal terminal return
    }
    return messages, nil
}
```

The `continue` re-enters the turn loop; the **existing** top-of-loop self-pull
(`humanInjector.Poll()` at the turn boundary, ADR-0022) injects the awaited message as a
user turn. Because `messages` (the full transcript) is carried forward untouched, history
is server-authoritative — the model sees the whole conversation, not a re-shipped prompt.

Invariants preserved:
- **ADR-0016 run-to-completion.** The injection is still a self-pull at a turn boundary; no
  cross-goroutine push into a mid-flight node.
- **Non-interactive is byte-identical.** `interactive=false` (every existing binding) hits
  the unchanged `return messages, nil`. A bus-less interactive loop also returns normally
  (`Wait` yields `errNoBus`), so the mode is a no-op without a bus.

### 2. Wake without polling (`internal/agent/humanmsg.go`)

The bus is `Poll`/`Drain` today (non-blocking). Parking needs an efficient block. Add a
per-node **cap-1 buffered notify channel**:

- `HumanBus.WaitForMessage(ctx, nodeID) error` — fast-path returns nil if a message is
  already queued (covers the enqueue-in-the-park-gap race); else drains any stale signal and
  blocks on the channel, returning nil on wake or `ctx.Err()` on cancel.
- `HumanInjector.Wait(ctx)` wraps it (the loop holds the injector, not the bus). A nil bus
  returns a sentinel `errNoBus` so the loop falls back to the terminal return.
- `Enqueue` calls `signalLocked(nodeID)` under the bus mutex, so the wake cannot race the
  queue append the waiter will drain. The channel is coalescing (one wake covers N enqueues;
  the waiter drains the whole queue).

This is orthogonal to `Poll`/`Drain` — a non-interactive loop never calls `WaitForMessage`,
so the bus is behaviorally unchanged for every existing caller.

### 3. Uncapped turns (`internal/runtime/inproc.go`)

An interactive loop **must** run with unlimited `maxTurns`. Each resumed exchange is real
turns, so the finite headless backstop a binding bakes into the agent (the loop-server
resolves `maxTurns` to 100 via `resolveMaxTurns`) would end the *entire conversation* once
cumulative turns crossed it. `Agent.SetMaxTurns(n)` (n≤0 = unlimited) is added, and
`StartLoop` calls it when `cfg.Interactive`:

```go
if cfg.Interactive {
    a.SetInteractive(true)
    a.SetMaxTurns(0)     // lift any finite per-run backstop the binding baked in
}
```

This is independent of a one-shot binding's per-turn backstop and doom-loop hook, which are
left alone.

### 4. Deterministic completion event (`internal/event`)

A new durable event kind:

```go
KindLoopParked Kind = "loop.parked"

type LoopParkedPayload struct {
    Turn    int    `json:"turn"`
    Content string `json:"content"`   // the terminal answer for this exchange
}
```

Emitted (step 1) immediately before the park. It is the **reliable** "exchange complete,
send the next message" marker: a client renders `Content` as the reply and re-enables input,
never guessing from stream shape. `KindLoopParked` is added to the pinned wire-format kind
list (the durable-format test that guards these strings). Only emitted in interactive mode;
a one-shot run never emits it. Older clients that don't know the kind simply ignore it (and
can still fall back to their heuristic).

### 5. Binding wiring (`internal/loopserver`, `internal/runtime`)

- `runtime.LoopConfig` gains `Interactive bool`.
- `loopserver` `startParams` gains `Interactive bool` (`json:"interactive,omitempty"`);
  `handleStart` passes it into `LoopConfig`. Stdio clients (binding #2) omit the field, so
  `Interactive` defaults false and their loops stay single-task — byte-identical.
- A WS chat client sets `"interactive": true` on `loop.start`.

No change to `loop.observe` / `loop.send` / `loop.event` framing, the HTTP replay endpoint,
or the `event.Event` envelope. `loop.parked` rides the existing `loop.event` push like any
other event.

## Client contract (informative)

A conversational client over binding #3:

1. `loop.start { task, model, interactive: true }` → `{ loop_id }`.
2. `loop.observe { loop_id, from_seq: 0 }` → live tail; render events as they stream.
3. On `loop.parked` → render `payload.content` as the answer, re-enable input. **This is
   the turn-complete signal.**
4. Follow-up: `loop.send { loop_id, input }` on the **same** `loop_id` → the parked loop
   wakes, runs the next exchange, emits its own `loop.parked`.
5. Reconnect is unchanged from 0048: track last `event.seq`, re-`observe` `from=<seq>`;
   subscribe-before-replay + dedup-at-watermark still hold across a parked loop.

The `examples/concierge-demo` app implements exactly this.

## Testing

- **Runtime** (`interactive_test.go`): (a) park→resume — start interactive, wait for
  `loop.parked`, `Send` on the same loop_id succeeds (no `ErrLoopFinished`), a second
  `loop.parked` arrives on the same stream; (b) the runtime lifts a **finite** baked-in cap
  (agents built with `maxTurns=1`, proving `SetMaxTurns(0)` override); (c) a non-interactive
  loop still finishes and rejects a later `Send`.
- **Loopserver**: a wire test that `"interactive": true` on `loop.start` reaches
  `LoopConfig.Interactive`, and that omitting it defaults false (stdio parity).
- **Event**: the durable kind-string pin includes `loop.parked`.
- **Concurrency**: the park/wake uses the bus mutex for the signal and a cap-1 coalescing
  channel; the fast-path check for an already-queued message closes the enqueue-in-the-gap
  race. `go test -race` on the agent + runtime packages.
- **Live**: per project policy, live verification uses the cheap gateway model (`glm`),
  never Claude. The concierge demo is the end-to-end live proof (multi-turn on one loop_id,
  context retained across turns).

## Open questions

- **Emit `loop.parked` for the TUI/CLI interactive path too?** The CLI already runs
  interactive with a human on a TTY; today it does not need a park event (the human drives
  turn cadence directly). Should the event be emitted for every interactive loop for
  uniformity, or only when a networked client needs the signal? (Current implementation:
  emitted whenever `interactive && humanInjector != nil`, which includes the CLI — harmless
  there, an extra event on the stream.)
- **Idle eviction of a parked loop.** A parked loop holds a goroutine + in-memory transcript
  indefinitely until ctx cancel. What is the idle-timeout / max-sessions policy, and does an
  evicted-then-resumed session need transcript rehydration from durable history (ties into
  the cross-instance-resume non-goal)? Deferred to 0049/0050.
- **Turn budget for a long conversation.** Interactive lifts the per-run cap entirely. Do we
  want a *per-exchange* turn budget (each user turn may take ≤N model turns) instead of fully
  uncapped, to keep the doom-loop backstop meaningful within a session?
- **Structured-delegation interaction.** An interactive root loop with `expectsSchema` set
  is nonsensical (a chat session is not a structured-return child). Should the runtime reject
  `Interactive` + expects, or is it simply never wired together by any binding?
- **`loop.send` while a turn is in flight.** Today the injector drains at the next turn
  boundary, so a mid-turn `Send` queues and is picked up after the current exchange rather
  than interrupting it. Is queue-until-next-boundary the desired chat semantics, or should a
  new message be able to interject/cancel the in-flight exchange?
