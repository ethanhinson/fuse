---
slug: replay-live-handoff-dedup-at-watermark
hook: "An observe seam that subscribes-to-live AND replays history double-delivers any event that lands between the two steps — subscribe first (drop nothing), then dedup at the replay watermark (drop live events with Seq <= last replayed); a plain sequential test cannot see it."
topics: [architecture, eventstore, concurrency, streaming]
changes: [45]
created: 2026-08-10
updated: 2026-08-10
promotion_state: candidate
promoted_to:
---

## Apply

A "catch up then go live" observer that both subscribes to the live stream *and* replays the durable log has a race: any event appended **between** the Subscribe and the Replay read is delivered twice — the replay emits it (it is now history) and the live subscription emits it (the subscriber was already attached when it landed). Their union is not disjoint by construction, and reordering the two steps only converts the duplicate into a *lost* event — you cannot order your way out of it.

Keep subscribe-before-replay (drop-nothing is the safe failure), then **dedup at the replay watermark**: track the highest `Seq` the replay emitted and drop any live event whose `Seq <= last`. The watermark is the single source of the boundary — do not pause appends or lock across the handoff. Test with a concurrent append forced into the gap; a sequential test cannot reproduce the interleaving.

Two adjacent seam obligations travel with this pattern: (1) close the event store on run completion so live subscriber channels close and the observe pumps terminate — but confirm the durable replay path opens its **own** reader so Attach-after-Close still works; (2) sending to an idle/finished loop should return a **distinguishable** error rather than silently stranding the message.

### Provenance

2026-08-10 — `loop.observe` in `internal/loopserver` (#45, PR #48), the headless second Runtime binding. The whole-branch review flagged this as the one Critical fix: replay→live handoff could deliver an event twice when an append landed between Subscribe and Replay. Fixed by deduping at the replay watermark (`ev.Seq <= last`), not by reordering or locking. The same review closed `Runtime.Send` to a finished loop (now `ErrLoopFinished`, mapped to a JSON-RPC error by `loop.send`) and added store-close-on-completion (with `fsstore.Replay` verified to open its own reader so durable Attach survives Close). The double-delivery was invisible until a test forced a concurrent append into the handoff gap.
