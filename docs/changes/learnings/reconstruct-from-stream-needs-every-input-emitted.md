---
slug: reconstruct-from-stream-needs-every-input-emitted
hook: "Before rebuilding model/session state by folding an event stream, verify EVERY state-mutating input is actually emitted as an event — user/human input frequently is NOT. If an input only mutates in-memory state (e.g. the injector appends a Role:user message to the transcript but emits no event), a fold from events alone silently drops it, and the gap is invisible until a byte-equal round-trip test catches it. Add the missing event kind (make the stream the complete source of truth) before writing the fold."
topics: [architecture, events, eventsourcing, runtime, reconstruction, streaming]
changes: [54]
created: 2026-08-15
updated: 2026-08-15
promotion_state: candidate
promoted_to:
---

## Apply

When a design decides to rebuild state from a durable event stream instead of persisting
that state separately (the "single source of truth is the event log" choice — event
sourcing in miniature), the fold is only lossless if the stream actually contains **every**
input that mutated the state. That is an assumption worth checking explicitly, because it is
easy to violate silently: an input path can mutate in-memory state directly without ever
emitting an event.

Concrete instance (change 0054, durable resumable sessions). The agent loop's model-facing
transcript (`messages`) is mutated by three kinds of input — the initial task, each assistant
response, and each tool result — but ALSO by human/user turns injected mid-conversation. The
assistant/tool inputs were emitted as events (`model.call.end`, `tool.call`, `tool.result`);
the user turns were **not** — the human injector just appended a `{Role:"user"}` message to
the in-memory transcript and moved on. So a fold from the event stream reconstructed a
transcript missing every user turn. Nothing failed loudly; the loop still ran. The gap only
surfaced under a round-trip test that folded a real loop's own durable events and asserted
**byte-equality** against the live in-memory transcript.

The fix that keeps the "no second source of truth" property intact: add the missing event
kind (`user.input`) and emit it wherever that input enters state, so the stream becomes the
complete record — rather than persisting the transcript separately (which would reintroduce
the parallel source of truth the design was avoiding). Guard it with the byte-equal
round-trip test, and feed the reconstruction the loop's OWN production events, never
hand-synthesized ones (see [[parity-test-feeds-each-side-its-own-production-source]]).

**Rule:** before writing any `events → state` fold, enumerate every state-mutating input and
confirm each has a corresponding emitted event. A missing one is a silent data-loss bug that
only a byte-equal round-trip test exposes. Related: reconstructing *completion* from stream
shape has the same class of hazard — see [[persistent-loop-needs-explicit-completion-event]].

## War story

**2026-08-15 (#54, PR #60)** — Grooming settled "rebuild the transcript from the durable
event stream, no second source of truth" (D1/D5). At build time, reconcile against
`origin/main` found the durable stream had no user-input event: `Agent.Run` seeded the
transcript from the task and the human injector's `Poll` appended user messages, but neither
emitted an event. The events→messages fold would therefore have dropped every user turn. Added
`KindUserInput` (`"user.input"`) emitted at the seed's user turns and each human Poll (with a
`SetSeeded` flag so a resumed loop does not re-emit turns already in the stream), making the
fold lossless. The byte-equal round-trip test (`TestReconstructRoundTripEqualsLiveTranscript`)
is what proves it and would have failed loudly had the gap remained.
