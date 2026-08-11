---
slug: persistent-loop-needs-explicit-completion-event
hook: "Once a run persists across turns (a loop that parks instead of finishing), a client can no longer infer 'this exchange is done, send the next message' from the SHAPE of the event stream — there is no run-end and no store-close between turns to key off. Emit an explicit completion event (e.g. loop.parked carrying the final answer) at the park boundary; reconstructing completion from stream shape desyncs the moment the one-shot lifecycle goes away."
topics: [architecture, events, runtime, lifecycle, streaming, ux]
changes: [53]
created: 2026-08-11
updated: 2026-08-11
promotion_state: candidate
promoted_to:
---

## Apply

A one-shot run gives clients a free completion signal: the run goroutine exits, the durable
registry marks it finished, the event store closes, live subscriber channels close. A client
can (and will) infer "the answer is ready" from that *shape* — the stream going quiet, the
store closing — without any dedicated "done" event.

The instant you make the run **persist across turns** — a loop that parks at the terminal
turn boundary and waits for the next human message instead of returning — every one of those
inferred signals disappears. There is no run-end, no store-close, no channel-close between
turns; the same `loop_id` and the same event stream carry turn N+1. A client keyed on stream
shape now hangs waiting for a quiescence that will never come, or fires early on an
intra-turn lull. Both are the same bug: completion was never a first-class fact, it was a
side effect of the one-shot lifecycle.

**Emit an explicit completion event** at the persistence boundary, carrying the payload the
client actually needs (the final turn's content), so the "exchange complete, send your next
message" signal is a fact on the wire, not a heuristic over the stream's silhouette. Two
adjacent obligations travel with a persistent conversational loop: (1) it must run with
**uncapped turns** — each resumed exchange consumes real turns, so a finite headless backstop
truncates the whole conversation at the cap; lift the cap when interactive mode is enabled,
independent of any per-turn backstop a one-shot binding wants; (2) keep the non-interactive
path byte-identical (run-to-completion preserved) so the new mode is purely additive.

Found the hard way: build a chat demo on a one-shot loop and the follow-up turn fails with
`loop finished`; fix the injector and the *next* failure is a UI hang — both trace to the
same missing primitive. Related: [[live-control-reads-state-at-decision-point]] (a "live"
surface must read state at the decision point, not snapshot a lifecycle assumption),
[[replay-live-handoff-dedup-at-watermark]] (the observe seam the persistent loop rides on).

## Provenance

2026-08-11 — Interactive loop mode in `internal/runtime` (#53, PR #51), carved out of the
0048 networked-binding demo. `loop.send` to a finished loop returned `ErrLoopFinished`; the
concierge demo's second turn failed with `runtime: loop finished`, and the follow-on UI hang
traced to the same root — a one-shot loop never had to emit a completion signal. Fixed by
parking at the terminal turn boundary (transcript carried forward, server-authoritative
history), waking via a cap-1 notify channel on the per-node human-message bus (ADR-0022), and
emitting a new `event.KindLoopParked` (`LoopParkedPayload{Turn, Content}`) just before the
park as the deterministic completion signal. Interactive loops run with uncapped `maxTurns`.
