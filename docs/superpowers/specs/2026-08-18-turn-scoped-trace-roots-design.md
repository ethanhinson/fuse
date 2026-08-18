<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0071 — Turn-scoped trace roots for interactive loops — end loop.run at first park, per-turn root spans](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0071-turn-scoped-trace-roots-interactive-loops.md)**
<!-- docket:backlink:end -->

# Turn-scoped trace roots for interactive loops — design

Change 0071 · Designed 2026-08-18 UTC (inline brainstorm with the human; superpowers:brainstorming unavailable in this environment)

## Problem

`internal/runtime/inproc.go` (`launchLoop`) starts one `fuse.loop.run` span when a loop
launches and ends it only when the run completes. Change #54 detached interactive
sessions from the request context: the run **parks** at turn-end and lives until the
idle-TTL reaper or an explicit end — commonly tens of minutes, potentially hours.

OTEL's BatchSpanProcessor exports a span only at `End()`. Consequences for a live
interactive session, observed directly in the wander demo:

- Child spans (`fuse.model.attempt.complete`, `fuse.tool.execute`, `fuse.spawn.child`)
  end promptly and export, but their **root does not exist in Tempo** until the session
  dies. The Traces Drilldown "Root spans" view filters the whole trace out; search shows
  an unknown root; duration is meaningless.
- An unclean process death (crash, SIGKILL) means the root **never** exports — a
  permanently rootless trace.
- There is no per-turn span at all, so turn boundaries are invisible even in a complete
  trace.

This is the known OTEL anti-pattern of scoping a root span to a session instead of a
bounded unit of work. The codebase already contains the correct idiom:
`StartFromCarrier(..., delayed=true)` (`internal/observe/otel/observer.go`) starts a
**new root with a causal link** for delayed/replayed work.

## Decisions

### D1 — Interactive `fuse.loop.run` ends at first park

For an interactive loop, the `loop.run` span ends (outcome `success`) when the session
**first parks** — it thereby covers startup plus the first turn and exports promptly.
One-shot loops are **byte-identical to today**: `loop.run` spans the whole run and ends
at completion. The reaper and every session-teardown path must defensively `End()` any
still-open span, and the end must be idempotent.

> Reconciled 2026-08-18: this paragraph originally asserted the handle "is already
> idempotent via `sync.Once`". It is not. `launchLoop` guards the end with a plain
> `ended bool` captured in its `end(out)` closure (`internal/runtime/inproc.go:327–334`),
> safe today only because exactly one goroutine calls it. D1 introduces a second caller
> (the park path) racing the run goroutine's completion, so the implementation must
> harden that guard — `sync.Once` or a mutex — rather than rely on existing behavior.

### D2 — Each subsequent turn is a new root: `fuse.loop.turn`

Every later Send→park cycle gets a `fuse.loop.turn` **root** span:

- Started with the provider-neutral equivalent of `WithNewRoot()` plus a **span link**
  to the `loop.run` span context — the context already persisted as the durable trace
  carrier (`agent.eventTrace`), so the link source is stable and survives resume. This
  is exactly the existing delayed-carrier idiom; no new OTEL surface is invented.
- Attributes: `loop_id`, `tenant` (normalized, same cardinality policy as today), and a
  monotonically increasing `fuse.turn.index` (1 = the turn inside `loop.run`).
- Child spans of the turn (model attempts, tool executes, spawns) parent to the turn
  span through the context, unchanged mechanically.
- A turn ends when the loop re-parks, the run completes, or the session is reaped;
  outcome derives from the turn's terminal condition exactly as `loop.run`'s does today.
- A **resumed** loop (post-reap `Send` → `Resume`) emits `fuse.loop.turn` roots linked
  to the carrier restored from the durable registry — the first resumed turn does NOT
  restart a `loop.run`.

### D3 — Turn boundary definition

A turn starts when the parked interactive loop wakes on an injected human message (or a
resume that carries one) and ends at the next park. The boundary is owned by the
runtime/agent park-wake seam (`launchLoop` + the park path), NOT inferred from stream
shape — the same principle as the SDK's `isCompletion` contract.

> Reconciled 2026-08-18: the two concrete seam points are (a) the **park**, already
> announced inside the agent at `internal/agent/loop.go:598` as `event.KindLoopParked`
> on the interactive no-tool-calls terminal path, immediately before
> `humanInjector.Wait(ctx)`; and (b) the **wake**, the runtime's own `Send` (and
> `Resume`, which carries its message). Both are observable from `internal/runtime`
> without adding a callback into `internal/agent`. The plan chooses between observing
> the parked event and adding an explicit runtime-side park hook, and must confirm the
> Send-side wake also covers the resume path.

### D4 — Tracing only; ADR-0045 untouched

The TUI's conversational-turn attribution (ADR-0045 timestamp bucketing) is explicitly
out of scope. If the turn span/event proves out, migrating ADR-0045's heuristic onto it
is a separate follow-up change. Metrics and logging projections are unchanged — this
change touches span topology only.

## Non-goals

- No change to one-shot trace shape, span names, or attributes.
- No change to the Observer contract's provider-neutrality (ADR-0040) beyond what
  `StartFromCarrier` already expresses; if a small contract extension is needed to say
  "new root linked to carrier" at the runtime seam, it reuses that method rather than
  adding a parallel one.
- No metrics for turns (span-derived TraceQL metrics cover it).
- No Tempo/Grafana config changes.

## Acceptance

1. **Live-session visibility**: with a live parked interactive session (wander), Tempo
   shows the first-turn trace (rooted at `fuse.loop.run`, ended) and one complete
   `fuse.loop.turn`-rooted trace per completed turn, each carrying `loop_id`/`tenant`/
   `fuse.turn.index` and a link to the session root — while the session is still alive.
2. **One-shot regression**: a one-shot run's exported span set is unchanged (asserted
   against the recorded span shape in existing observability tests).
3. **Reap/teardown**: reaping a parked session leaves no un-ended spans (the open-span
   defensive End fires; no rootless fragments beyond the at-most-one in-flight turn).
4. Full suite green; live verification against a cheap gateway model, never Claude.
