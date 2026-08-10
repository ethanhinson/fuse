<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0043 — Runtime EventStore seam — typed, pluggable, introspectable loop event stream](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0043-runtime-eventstore-seam.md)**
<!-- docket:backlink:end -->

# Spec 0043 — Runtime EventStore seam: a typed, pluggable, introspectable loop event stream

## Problem

The agent loop's activity is not observable through any single, typed, subscribable
surface, and it produces no durable record that a consumer could replay or that a future
change could resume from. Today the loop's activity leaks out through three unrelated,
partial channels:

- **Forensic session log** — `session.Logger.Write(LogEntry)` (`internal/session/log.go:93`)
  appends coarse per-node lifecycle entries (`{TS, NodeID, ParentID, Label, Depth, Kind}`,
  `log.go:20-27`) to `<baseDir>/<sessionID>/session.jsonl`. It is **write-only**: the
  package exposes no reader, and nothing subscribes. It records *that a node ran*, not the
  loop's turn-by-turn activity.
- **Segment store** — the Tier-2 summarizer archives pre-summary message regions via
  `segmentSink.Archive(SegmentRegion)` (`internal/agent/loop.go:412-424`, ADR-0018/0020),
  a gzip-compressed record of *summarized-away* context. It captures a slice of history
  for recovery, not the live activity stream.
- **Internal channels** — subagent results flow back to the parent through the
  `AgentHandle` channel carrying `SpawnDone{Result, Structured}`. This is in-process,
  point-to-point, and invisible outside the spawn machinery.

No consumer — a TUI, an external binding, a durability layer — can subscribe to "everything
the loop is doing" as one typed stream. The loop calls `Complete()`, dispatches tools, spawns
children, summarizes context, trips the loop detector, and errors, but each of those state
transitions is either logged coarsely, buried in a channel, or not surfaced at all.

This is the **first of three changes** toward a `Runtime` seam (fuse as a portable
fan-out + model-mesh loop engine behind a stable, pluggable interface). The three, in
dependency order:

1. **`runtime-eventstore-seam` (this change)** — a typed `Event`, a pluggable
   `EventStore` interface (`Append`/`Subscribe`/`Replay`), a JSONL impl, and loop
   emission at every state transition. Additive by construction; the session log is
   re-expressed as a *consumer* of the stream.
2. **`spawn-handle-async` (next)** — change `SpawnFunc` from synchronous `(string, error)`
   to a handle-returning contract; the child's result arrives as an **event** on this
   stream. Depends on this change.
3. **`runtime-interface-and-binding`** — extract the named `Runtime` interface
   (`StartLoop`/`Send`/`Spawn`/`Observe`/`Attach`) over the now-async engine and stand up
   a second loop-control binding to prove policy-freedom. Depends on the second.

## Scope of this change (hard boundaries)

**In scope.** A typed `Event`; the agent-free `EventStore` interface; a JSONL-backed impl
in the per-session dir; a process-global lock-guarded holder to inject it; loop emission at
every state transition; an additive `Observe`/`Subscribe` read path; and re-expressing the
session log as a *consumer* of the event stream.

**Out of scope — deferred to a named later change, do not do here.**

- **`SpawnFunc` signature stays `(string, error)`.** Changing it to a handle is
  `spawn-handle-async` (change 2). This change adds subagent *lifecycle events* to the
  stream but does **not** touch the spawn contract's shape.
- **The tool-call MCP server (`cmd/fuse/mcp_server.go`) is untouched.** The loop-control
  binding is change 3, and it is a *new, higher* surface — not a modification of the
  existing tool-call one.
- **No reattach/resume implementation.** `Replay(from)` exists and is the durable
  foundation, but wiring it to a restarted process is a later change. This change makes
  loops *replayable in principle*, not resumable in fact.
- **No networked EventStore backend.** JSONL only. The interface leaves room
  (ADR-0017-style), exactly as the segment store does.
- **Deleting the old direct `Logger.Write` call sites.** The log is re-expressed as a
  consumer here and produced two ways *transiently* (verified identical); removing the
  now-redundant direct writes is a trivial follow-up once equivalence is proven, so this
  PR stays green-by-construction.

## Target design

The committed target is a **fully robust, introspectable stream**: every loop state
transition is a first-class, typed, subscribable event carrying **full payloads**, and the
model-call boundary is filled in with **streaming token deltas**. The `Event` schema is
designed for that target from day one so no interface rework is needed later; the build
**stages** emission (see *Build stages*) so the additive core lands without dragging the
model-adapter work along.

### The `Event` type (agent-free package)

A discriminated union over a `Kind`, mirroring the segment store's agent-free/impl split
(ADR-0017): the type + interface live in an agent-free leaf package (proposed
`internal/event`), the fs impl in a subpackage (proposed `internal/event/fsstore`) that may
import `internal/agent`.

```go
// internal/event — agent-free leaf package
type Kind string

const (
    KindTurnStart      Kind = "turn.start"
    KindTurnEnd        Kind = "turn.end"
    KindModelCallStart Kind = "model.call.start"
    KindModelDelta     Kind = "model.delta"      // streaming token delta (Stage B)
    KindModelCallEnd   Kind = "model.call.end"
    KindToolCall       Kind = "tool.call"
    KindToolResult     Kind = "tool.result"
    KindSpawnStart     Kind = "spawn.start"
    KindSpawnDone      Kind = "spawn.done"
    KindSummarize      Kind = "context.summarize"
    KindLoopTrip       Kind = "loop.detector.trip"
    KindError          Kind = "error"
)

type Event struct {
    Seq      uint64          // monotonic per session; the Replay cursor
    TS       time.Time
    NodeID   string          // which agent in the spawn tree (matches LogEntry.NodeID)
    ParentID string
    Depth    int
    Turn     int
    Kind     Kind
    Payload  json.RawMessage // kind-specific; full payload (see coverage)
}
```

`Payload` is a per-kind struct marshaled to `RawMessage` so the envelope is stable and
consumers decode only the kinds they care about. **Full-payload coverage** (the committed
target): `model.call.end` carries the complete assistant response; `tool.call`/`tool.result`
carry full args and full results; `spawn.done` carries `SpawnDone{Result, Structured}`;
`model.delta` carries the incremental token text. The envelope's `Seq` is the single
monotonic cursor that makes `Replay(from Seq)` and de-dup deterministic.

### The `EventStore` interface (agent-free)

```go
// internal/event — agent-free
type EventStore interface {
    Append(Event) error                       // best-effort; loop never fails on emit
    Subscribe() (<-chan Event, func())        // live tail from now; returned func unsubscribes
    Replay(from Seq) ([]Event, error)         // durable history from a cursor (0 = all)
}
```

- **`Append`** is best-effort and non-blocking from the loop's perspective — identical
  contract to `segmentSink.Archive`: an error is logged and the turn continues, never fails
  (`loop.go:419-423` is the precedent).
- **`Subscribe`** returns a live channel plus an unsubscribe closure. Multiple concurrent
  subscribers are supported (the TUI, a binding, the log-consumer). **The channel must
  never block the loop** — a slow/absent consumer is dropped or buffered, never
  back-pressures `Append` (this preserves ADR-0016's slot-yield/no-deadlock invariant: a
  subscriber can never wedge an agent's scheduler slot).
- **`Replay`** reads durable history from the JSONL. `Subscribe` + `Replay(from)` together
  are what a future reattach uses: replay to the last-seen `Seq`, then tail live.

### The JSONL impl (`internal/event/fsstore`)

Mirrors `FSSegmentSink` exactly (`internal/segment/fssink/sink.go`):

```go
func NewFSEventStore(baseDir, sessionID string) *FSEventStore
```

Writes to `<baseDir>/<sessionID>/events.jsonl` (per-session dir, ADR-0018), one JSON
`Event` per line, `Seq`-ordered, flushed on append. Inherits the segment store's durability
philosophy (append-only, non-destructive — ADR-0020); gzip/rotation is **not** in scope but
the on-disk format leaves room for it. Internal mutation (the `Seq` counter, the file
handle) is lock-guarded, as `FSSegmentSink` guards its `seq`/index.

### Process-global holder + injection

Mirror `activeSegmentSink` exactly (`cmd/fuse/run.go:266-293`): a package-level
`activeEventStore` var + `sync.RWMutex`, with `setActiveEventStore(EventStore)` /
`currentEventStore() EventStore`. Installed **once** at session startup next to
`setActiveSegmentSink(...)` (`shell.go:169`), after the root `AgentNode` exists so the
`sessionID` is known. One-shot / probe / MCP paths that never install it get a nil store →
a no-op default (same graceful degradation as the segment sink).

The Agent gets an `eventSink EventStore` field wired **after** `New()` via a setter, exactly
like `EnableSummarization` threads `segmentSink` (`agent.go:222-227`); the setter is called
inside the post-`New` wiring in `buildAgentCore` (`run.go:797-836`), pulling from
`currentEventStore()`. A nil sink is a no-op — no emission point ever nil-panics.

### Loop emission (robust, per-transition)

The loop emits at **every** state transition, not only where it logs today. Concretely, in
`Agent.Run` (`internal/agent/loop.go:349`+): `turn.start`/`turn.end` around the turn loop
(`loop.go:382`); `model.call.start`/`model.call.end` around `a.model.Complete()`
(`loop.go:485`); `tool.call`/`tool.result` around each tool dispatch; `spawn.start`/
`spawn.done` at the spawn boundary; `context.summarize` beside the existing
`segmentSink.Archive` (`loop.go:412`); `loop.detector.trip` where `loopDetector` fires
(`loop.go:351`); `error` on any turn-terminating error. Emission is **explicit greppable
`a.eventSink.Append(Event{...})` calls** at each point — matching the `segmentSink.Archive`
pattern, deliberately not a hidden central interceptor, so the set of emitted transitions is
auditable by grep.

### Session log re-expressed as a consumer ("log adapts")

`Event` becomes the **canonical** record; the forensic session log becomes a **projection**
of the stream. A small `LogEntry`-projecting subscriber consumes `Subscribe()` and writes the
*exact current* `session.jsonl` format (`LogEntry`, `log.go:20-27`) — so every existing
reader of `session.jsonl` is byte-unaffected. The log "adapts" by being *derived from*
events rather than written independently.

To keep this PR green-by-construction, the direct `Logger.Write` call sites are **retained**
during this change; the projected log is produced alongside and verified identical, and the
now-redundant direct writes are deleted in a trivial follow-up. No capability is deferred —
only dead-code removal.

## Build stages (preserves the de-risking property)

The `Event` schema is designed for the full target (full payloads + deltas) up front, so
staging costs nothing architecturally — Stage A simply does not *emit* the delta kind yet.

- **Stage A — additive core (no adapter changes; lands green).** `Event` type (delta-ready),
  `EventStore` interface, `FSEventStore` JSONL impl, global holder + injection, loop emits
  full-lifecycle **boundary** events with **full payloads** (`model.call.end` with complete
  response, full tool args/results, `spawn.done`, summarize, loop-trip, error), and the
  session log re-expressed as a consumer. Fully additive: no `Completer`/adapter change, no
  spawn-contract change, MCP server untouched.
- **Stage B — streaming token deltas (touches the provider boundary; fenced).** Add a
  streaming path to `Completer` (a streaming sibling to `Complete()`) and implement it in
  **both** adapters — the LiteLLM HTTP `Adapter` and the `cli/` Claude-subprocess
  `CLIAdapter` — then emit `model.delta` events between `model.call.start` and
  `model.call.end`. This is the only non-additive part; it is fenced as its own stage so
  that if it proves gnarlier than expected it can slip to its own change **without blocking
  the seam** the other two changes depend on.

## Open questions (resolve during planning)

- **Segment-store overlap (must resolve).** Full-payload events mean `events.jsonl` contains
  the complete transcript — every message, every tool result — which **overlaps the segment
  store's job** (ADR-0020: gzip-archived pre-summary messages). This is a real architectural
  interaction, not a duplication to wave through. Two coherent resolutions: (a) events are
  the single complete record and **segments become a derived, compressed view** over events;
  or (b) events carry pointers to segment/message storage for heavy content and stay lean.
  The plan must pick one deliberately; `related: [30]` (segment-store) is set for this reason.
  Note the target design above commits to full payloads — resolution (a) is the default
  reading, but the plan owns the final call and its storage-cost consequences.
- **`Seq` allocation across the spawn tree.** Is `Seq` per-session-global (one counter for
  the whole tree, requiring a shared allocator) or per-node (each agent's own sequence,
  namespaced by `NodeID`)? Per-session-global gives a single total order for `Replay` but
  needs a process-wide monotonic counter; per-node is simpler to allocate but a consumer must
  merge streams. Lean per-session-global for a clean replay cursor; confirm against the
  in-process-only constraint (a networked store would revisit this).
- **Subscriber back-pressure policy.** When a subscriber's channel is full, drop-oldest,
  drop-newest, or unbounded-buffer? Must never block the loop (ADR-0016). Lean bounded
  buffer + drop-newest-with-a-gap-marker, so a slow consumer degrades visibly rather than
  silently.
- **`Observe` handle identity.** This change has no `LoopHandle` yet (that's change 3).
  `Observe` here is `Subscribe()` on the installed store, session-scoped. Confirm the shape
  is forward-compatible with a per-loop handle in change 3 (it should be: a handle just
  narrows `Subscribe` to a `NodeID` subtree).

### Open questions — RESOLVED at reconcile (2026-08-10)

The plan is bound to these resolutions (see the change's `## Reconcile log` for rationale):

- **Segment-store overlap → (b) lean/independent.** `events.jsonl` carries full boundary
  payloads, born plaintext + append-only; the **segment store is left untouched**. Unifying
  (resolution (a)) would be a risky refactor of ADR-0020's proven non-destructive store and is
  *not* de-risking Stage A work. Transient content overlap accepted; a later change may dedupe.
- **`Seq` → per-session-global, allocated inside the store under its lock.** The per-session
  store instance is the monotonic allocator; `Append` assigns `Seq` under the file-handle lock.
  Callers never pre-set `Seq`. Single total order → clean `Replay(from)` cursor.
- **Back-pressure → bounded per-subscriber buffer + drop-newest-with-gap-marker.** Delivery is
  a non-blocking send; a full buffer drops the newest event for that subscriber and raises a gap
  marker. `Append` and the loop never block on a subscriber (ADR-0016 preserved); guarded by a
  non-blocking-send regression test.
- **`Observe` → session-scoped `Subscribe()`**, forward-compatible with a per-loop `NodeID`
  narrowing in change 3.

**Anchor note:** the `EventStore` type + interface live in the agent-free leaf `internal/event`
(cleaner than the segment precedent, whose `SegmentSink` interface sits in `internal/agent`
because `SegmentRegion` embeds `[]model.Message` — `Event` has no such dependency). The single
current direct `Logger.Write` site is `cmd/fuse/shell.go` (child-completion "done"/"error"),
which the log-as-consumer projection reproduces; `SpawnDone` is `{Result, Err, Structured}`.

## Verification

- **Additive-ness:** the full existing suite passes with Stage A landed and **no existing
  behavior changed** — same `session.jsonl` bytes (projected log ≡ direct log), same segment
  archival, same spawn results.
- **Round-trip:** `Append` then `Replay(0)` returns the same events in `Seq` order; a
  `Subscribe` taken before a run receives the live stream and a late `Subscribe` +
  `Replay(from)` reconstructs the full history to the cursor.
- **Non-blocking:** a deliberately-stalled subscriber does not wedge the loop or an agent's
  scheduler slot (ADR-0016 regression guard).
- **Coverage:** every enumerated `Kind` is emitted by a driven run that exercises a
  tool call, a subagent spawn, a Tier-2 summarization, and an induced error.
