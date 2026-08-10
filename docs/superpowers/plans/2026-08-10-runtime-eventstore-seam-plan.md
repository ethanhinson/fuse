<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0043 — Runtime EventStore seam — typed, pluggable, introspectable loop event stream](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0043-runtime-eventstore-seam.md)**
<!-- docket:backlink:end -->

# Plan 0043 — Runtime EventStore seam

> Spec: `docs/superpowers/specs/0043-runtime-eventstore-seam.md` (on `docket`).
> Change: `docs/changes/active/0043-runtime-eventstore-seam.md` (on `docket`).
> Reconciled 2026-08-10 — anchors verified against `main`, open questions resolved.

## Bound decisions (from the reconcile)

- **Package split.** `internal/event` is the **agent-free leaf** holding `Kind`, `Event`,
  the per-kind payload structs, and the `EventStore` interface. `Event.Payload` is
  `json.RawMessage` so the package imports neither `internal/agent` nor `internal/model` —
  cleaner than the segment precedent (whose `SegmentSink` sits in `internal/agent` because
  `SegmentRegion` embeds `[]model.Message`). The fs impl lives in the subpackage
  `internal/event/fsstore`, mirroring `internal/segment/fssink`. The process-global holder +
  injection live in `cmd/fuse`.
- **Seq.** Per-session-global, allocated **inside the store** under its lock in `Append`.
  Callers never pre-set `Seq`. Single total order → clean `Replay(from)` cursor.
- **Back-pressure.** Each `Subscribe()` gets its own buffered channel; delivery is a
  **non-blocking send**; on a full buffer the store **drops the newest** event for that
  subscriber and raises a **gap marker** so the drop is visible. `Append` and the loop never
  block on a subscriber (ADR-0016 preserved). Guarded by a non-blocking-send regression test.
- **Segment-store overlap.** Resolution (b): `events.jsonl` carries full boundary payloads,
  born **plaintext + append-only**; the segment store is **left untouched**. No unification.
- **Stage posture.** Stage A (§Tasks 1–8) is the must-land, fully-additive, green-by-
  construction body. Stage B (§Tasks 9–11) is fenced streaming deltas; land Stage A cleanly
  first and only fold Stage B in if it lands without dragging Stage A red — else defer it to
  its own change (note in PR + results).

## Verified anchors (current `main`)

- `internal/session/log.go` — `LogEntry{TS,NodeID,ParentID,Label,Depth,Kind}`, `Logger.Write`.
- Segment mirror: `agent.SegmentSink` iface + `SegmentRegion` in `internal/agent/segment.go`;
  `FSSegmentSink` + `NewFSSegmentSink(baseDir, sessionID)` in `internal/segment/fssink/sink.go`
  (mutex-guarded, born-gzip; **we do NOT gzip events**).
- Holder: `activeSegmentSink` + `activeSegmentSinkMu sync.RWMutex`, `setActiveSegmentSink`,
  `currentSegmentSink` in `cmd/fuse/run.go`.
- Install: `cmd/fuse/shell.go` — `setActiveSegmentSink(fssink.NewFSSegmentSink(logDir, tree.RootID()))`.
- Post-`New` wiring: `installSummarizer` → `a.EnableSummarization(..., currentSegmentSink())`
  in `cmd/fuse/run.go`.
- Loop (`internal/agent/loop.go`): `Run` @349; turn loop @382; `a.model.Complete()` @485;
  `executeTools` @596; `segmentSink.Archive(...)` @412; `newLoopDetector` @351 / `detector.seen`
  @572; error returns @384/@464/@499/@587/@599.
- Single direct `Logger.Write` site: `cmd/fuse/shell.go` @329 — child completion, Kind
  "done"/"error", keyed to `childNode`.
- Adapters: `internal/model/adapter.go` `Adapter.Complete` @256; `cmd/fuse/cli_adapter.go`
  `CLIAdapter.Complete` @55. `Completer` iface in `internal/agent/agent.go` @11.
- Spawn: `SpawnFunc(ctx, SpawnRequest)(string,error)` `internal/tools/spawn_agent.go`;
  `AgentHandle`, `SpawnDone{Result, Err, Structured}` `internal/agent/spawn.go`.

Test suite: `go test ./...` (Makefile `test`); race: `make test-race`. Mirror
`internal/segment/fssink/sink_test.go` structure (t.TempDir, table tests).

---

## TDD discipline

Every task is test-first: write the failing test, run it red, implement, run it green, then
the full `go test ./...` before moving on. Stage A must keep the whole suite green at every
task boundary (additive-by-construction). Concurrency tasks additionally run under `-race`.

---

## Stage A — additive core (must land green)

### Task 1 — `internal/event`: the `Event` schema (agent-free leaf)

**Files:** `internal/event/event.go`, `internal/event/event_test.go`

- Define `type Seq uint64`, `type Kind string`, and the `Kind` consts exactly as the spec
  enumerates (`turn.start`, `turn.end`, `model.call.start`, `model.delta`, `model.call.end`,
  `tool.call`, `tool.result`, `spawn.start`, `spawn.done`, `context.summarize`,
  `loop.detector.trip`, `error`). Include `model.delta` now (delta-ready schema) even though
  Stage A never emits it.
- Define the envelope:
  ```go
  type Event struct {
      Seq      Seq             `json:"seq"`
      TS       time.Time       `json:"ts"`
      NodeID   string          `json:"node_id"`
      ParentID string          `json:"parent_id,omitempty"`
      Depth    int             `json:"depth,omitempty"`
      Turn     int             `json:"turn"`
      Kind     Kind            `json:"kind"`
      Payload  json.RawMessage `json:"payload,omitempty"`
  }
  ```
- Define per-kind payload structs (marshaled into `Payload`): `TurnStartPayload{Turn}`,
  `TurnEndPayload{Turn}`, `ModelCallStartPayload{Model, MsgCount}`,
  `ModelCallEndPayload{Content, InputTokens, OutputTokens, ToolCalls []ToolCallRef}`,
  `ModelDeltaPayload{Text}`, `ToolCallPayload{ID, Name, Args json.RawMessage}`,
  `ToolResultPayload{ID, Name, Result, IsError}`, `SpawnStartPayload{ChildNodeID, Label, Task}`,
  `SpawnDonePayload{ChildNodeID, ParentID, Label, Depth, Result, Err, Structured json.RawMessage}`,
  `SummarizePayload{TurnStart, TurnEnd, ToolNames, TokensBefore, TokensAfter, Pointer}`,
  `LoopTripPayload{Turn}`, `ErrorPayload{Err, Turn}`. Keep them **agent/model-free** —
  reference no `model.Message` etc.; heavy content is carried as strings / `RawMessage`.
- Helper constructors `NewEvent(kind, ...)`? No — keep it dumb; provide a small
  `MarshalPayload(any) (json.RawMessage, error)` helper and a typed `DecodePayload[T]` is not
  needed (consumers `json.Unmarshal` themselves). Add `func (e Event) MarshalJSONL() ([]byte, error)`
  and `func ParseEvent(line []byte) (Event, error)` for the self-delimiting round-trip
  (learning: self-delimiting-serialization — each line is one JSON object; payload escaping is
  inherent, no in-band header lines).

**Tests:** marshal/unmarshal round-trip of each payload kind through `Event`; a payload whose
string body contains newlines and `key:`-shaped lines round-trips byte-exact (guards the
self-delimiting property); `Kind` consts have the exact string values (pin the wire format).

### Task 2 — `internal/event`: the `EventStore` interface + a no-op default

**Files:** `internal/event/store.go`, `internal/event/store_test.go`

- Define the interface exactly:
  ```go
  type EventStore interface {
      Append(Event) error
      Subscribe() (<-chan Event, func())
      Replay(from Seq) ([]Event, error)
  }
  ```
- Provide `type NoopStore struct{}` implementing it (Append no-op nil, Subscribe returns a
  closed/empty channel + no-op cancel, Replay returns nil,nil) — the nil-holder default so no
  emission point ever nil-panics (mirrors the segment no-op).

**Tests:** `NoopStore` satisfies `EventStore` (compile-time `var _ EventStore = NoopStore{}`);
Append/Subscribe/Replay on Noop are safe and inert.

### Task 3 — `internal/event/fsstore`: JSONL-backed `FSEventStore`

**Files:** `internal/event/fsstore/store.go`, `internal/event/fsstore/store_test.go`

Mirror `FSSegmentSink` structure, but **plaintext append-only** (no gzip) and add the
subscriber fan-out:

- `type FSEventStore struct { baseDir, sessionID string; mu sync.Mutex; seq event.Seq; f *os.File; w *bufio.Writer; subs map[int]*subscriber; nextSub int }`
  where `subscriber{ ch chan event.Event; dropped bool }`.
- `func NewFSEventStore(baseDir, sessionID string) (*FSEventStore, error)` — opens (creates)
  `<baseDir>/<sessionID>/events.jsonl` for append (use `session.SessionDir` for the path; the
  file is opened `O_CREATE|O_WRONLY|O_APPEND`). Lazy dir creation like the segment sink.
- `Append(e event.Event) error`: under `mu`, assign `s.seq++; e.Seq = s.seq` (Seq allocated
  here — callers pass Seq=0), set `e.TS` if zero, marshal via `event`'s JSONL encoder, write +
  flush. Then fan out to each subscriber with a **non-blocking send**
  (`select { case sub.ch <- e: default: sub.dropped = true /* gap marker */ }`). A write error
  is returned (best-effort at the call site) but a subscriber-full drop is NOT an error.
- `Subscribe() (<-chan event.Event, func())`: under `mu`, make a buffered channel
  (cap = `subBuffer`, e.g. 256), register it, return it + an unsubscribe closure that removes
  it and closes it (idempotent).
- `Replay(from event.Seq) ([]event.Event, error)`: open the file for read, scan line-by-line
  (`bufio.Scanner` with a large buffer for big payloads), `event.ParseEvent` each, filter
  `Seq > from` (from=0 ⇒ all). Independent of the write handle (durable history).
- Gap marker: expose it via a synthesized `error`-kind event or a `Dropped()` count? Keep
  minimal for Stage A: when a subscriber had a drop, the NEXT successful send is preceded by a
  best-effort gap-marker event (`Kind == KindError`, payload `{"gap":true}`) OR simpler: track
  `sub.dropped` and, on unsubscribe or on the next deliverable event, emit one gap event then
  clear. Implement the simplest visible-drop that a test can assert; document the choice.

**Tests (mirror `sink_test.go`):**
- `Append` then `Replay(0)` returns the same events in Seq order (byte-exact payloads).
- `Replay(from)` returns only `Seq > from`.
- Seq is monotonic and assigned by the store (caller Seq ignored).
- Lazy dir creation.
- **Non-blocking (the ADR-0016 guard):** a subscriber whose buffer is full does NOT block
  `Append` — fill the sub's buffer, then run many `Append`s in a bounded time and assert they
  all return promptly; assert a gap/drop was recorded. Run under `-race`.
- **Concurrent Append** from N goroutines yields N well-formed lines, no corruption, monotone
  Seq (mutex guard) — run under `-race`.
- Subscribe-before-run receives the live stream; unsubscribe closure stops delivery and is
  idempotent.

### Task 4 — process-global holder + injection in `cmd/fuse`

**Files:** `cmd/fuse/run.go` (holder + wiring), `cmd/fuse/run_test.go` or a focused test.

- Add `activeEventStore event.EventStore` + `activeEventStoreMu sync.RWMutex`,
  `setActiveEventStore(event.EventStore)`, `currentEventStore() event.EventStore` — mirror
  `activeSegmentSink` exactly (ADR-0019: lock-guarded, one-process-one-session).
- `currentEventStore()` returns `event.NoopStore{}` when unset (not nil) so every read is
  safe — OR returns nil and the Agent setter no-ops on nil. Pick **NoopStore default** for
  symmetry and to make the Agent field never-nil.

**Tests:** set/get round-trip under the mutex; default is a usable no-op store.

### Task 5 — Agent `eventSink` field + post-`New` setter

**Files:** `internal/agent/agent.go`, `internal/agent/loop.go` (field use), agent test.

- Add `eventSink event.EventStore` to `Agent` (defaulting to `event.NoopStore{}` in `New` so
  it is **never nil**). `internal/agent` importing `internal/event` is safe (leaf, agent-free —
  no cycle).
- Add `func (a *Agent) SetEventSink(s event.EventStore)` (no-op guard: nil ⇒ install NoopStore).
  Threaded exactly like the segment sink is via `EnableSummarization`.
- Add a tiny private helper `func (a *Agent) emit(kind event.Kind, turn int, payload any)` that
  builds the envelope (NodeID/ParentID/Depth from the Agent's node identity if available — see
  note below), marshals the payload, and calls `a.eventSink.Append(...)` **best-effort**
  (log-and-continue on error, exactly like `segmentSink.Archive`). `emit` is the single greppable
  helper; each call site passes an explicit `Kind` so the emitted set stays grep-auditable.
  - **Node identity:** the Agent today does not carry its own `NodeID`/`ParentID`/`Depth`
    (those live on `AgentNode` in the tree, known at the `cmd/fuse` spawn site). Add
    `nodeID, parentID string; depth int` fields to `Agent` set via a `SetNodeIdentity` setter
    called from the same post-`New` wiring that knows the node (root at `shell.go`, children in
    the spawn closure). When unset (probe/one-shot), identity is empty strings / 0 — harmless.

**Tests:** `New` leaves a non-nil no-op sink; `SetEventSink(nil)` keeps a no-op; `emit` with a
recording fake store captures the right envelope fields; `SetNodeIdentity` populates envelopes.

### Task 6 — loop emission at every transition (boundary events, full payloads)

**Files:** `internal/agent/loop.go`, `internal/agent/loop_test.go`.

Insert explicit `a.emit(...)` calls (greppable) at each anchor — Stage A boundary set:

- `turn.start` at top of the `for turn` body (after ctx check), `turn.end` at the end of each
  iteration (both success-continue and the return paths — emit `turn.end` before every
  `return messages, ...` inside the loop, and before `return messages, nil` on no-tool-calls).
- `model.call.start` immediately before `a.model.Complete(ctx, req)` (payload: model id,
  message count); `model.call.end` immediately after a successful complete (payload: full
  `resp.Content`, input/output tokens, tool-call refs). On a model error, emit `error` (payload
  err+turn) before `return messages, err` @499.
- `tool.call` for each `resp.ToolCalls` entry before dispatch, and `tool.result` for each tool
  result after `a.executeTools(...)` @596 (full args, full results, IsError). (executeTools
  returns the result messages — derive per-call results from them or emit inside a thin wrapper;
  choose the site that gives full args+results without changing executeTools' signature.)
- `context.summarize` right beside the existing `segmentSink.Archive(...)` @412 (payload: turn
  span, tool names, tokensBefore/After, pointer).
- `loop.detector.trip` where `detector.seen(fps)` fires @572 (payload: turn).
- `error` before `return messages, err` at the context-too-large @464 and max-turns @599 exits,
  and the ctx-cancel @384.
- `spawn.start` / `spawn.done`: these straddle `cmd/fuse` (the spawn closure), NOT the loop —
  see Task 7. The loop itself does not spawn; the spawn tool does. So spawn events are emitted
  at the `cmd/fuse` spawn boundary via `currentEventStore()`.

**Tests:** drive `Agent.Run` with a scripted `Completer` fake + a fake `ToolExecutor` and a
recording `EventStore`; assert the emitted `Kind` sequence for: (a) a plain no-tool turn
(`turn.start, model.call.start, model.call.end, turn.end`); (b) a tool-call turn
(`... tool.call, tool.result, turn.end` then next turn); (c) an induced model error
(`... error`); (d) a max-turns cap (`... error` with ErrMaxTurns). Assert payloads carry full
content (not truncated). This is the spec's **Coverage** verification for the loop-local kinds.

### Task 7 — spawn lifecycle events at the `cmd/fuse` spawn boundary

**Files:** `cmd/fuse/shell.go` (spawn closure), and the other two cloned child builders per
learning **patch-every-cloned-child-builder** (`cmd/fuse/main.go` one-shot `run()` and
`cmd/fuse/research_probe.go`) — grep `makeSpawnFunc`/child-run sites at fix time and patch every
one.

- At the start of a child run (in the spawn closure, where `childNode` is known), emit
  `spawn.start` via `currentEventStore()` with `{ChildNodeID, Label, Task}`.
- After `a.Run(ctx, history)` returns, emit `spawn.done` with
  `{ChildNodeID, ParentID, Label, Depth, Result, Err, Structured}` — the SAME data the direct
  `sessLog.Write(LogEntry{...})` uses, so the log projection (Task 8) can reproduce the entry.
  Keep the direct `sessLog.Write` in place (green-by-construction).
- Set the child agent's node identity (`a.SetNodeIdentity(childNode.ID, childNode.ParentID,
  childNode.Depth)`) and its event sink (`a.SetEventSink(currentEventStore())`) in the same
  post-build wiring block that already calls `SetStripSpawn`/`SetExpects`. Do the equivalent for
  the root agent at its build site.

**Tests:** a `cmd/fuse`-level test (or extend an existing spawn test) that runs a child through
the spawn closure with a recording store installed via `setActiveEventStore` and asserts
`spawn.start` then `spawn.done` with the full payload; assert the child's loop events also land
(identity populated). Where a full cmd/fuse harness is heavy, assert at the closure seam.

### Task 8 — session log re-expressed as a consumer (log-as-consumer)

**Files:** `cmd/fuse/` new file `event_log_consumer.go` (or `internal/session/` a projector that
takes an `event`-shaped input — but keep it in `cmd/fuse` to avoid `session`→`event` coupling;
`session` stays agent-free). Test alongside.

- Add a projector: `func projectEventToLog(e event.Event) (session.LogEntry, bool)` that maps a
  `spawn.done` event → `LogEntry{TS, NodeID:ChildNodeID, ParentID, Label, Depth, Kind:"done"|"error"}`
  (Kind "error" when the payload's `Err != ""`). Returns `false` for kinds the log does not
  project (everything except spawn.done) — matching the CURRENT log which only records child
  completion.
- Wire a consumer at session startup next to `setActiveEventStore`: `ch, cancel := store.Subscribe()`
  then a goroutine ranges `ch`, projects, and writes to a SECOND logger (a temp/parallel file
  during this change) so equivalence can be asserted **without** disturbing the shipped
  `session.jsonl`. Per the spec, both run transiently; the direct write stays canonical for now.
  Stop the consumer on session teardown (cancel + drain).

**Tests (the additive-ness gate):** feed a known `spawn.done`/error event stream through the
projector and assert the projected `LogEntry` marshals **byte-identical** to what the direct
`sessLog.Write` path produces for the same `childNode` (TS handling: inject a fixed TS so the
comparison is deterministic). This proves the projection ≡ direct log; the dead-write deletion is
the named trivial follow-up (out of scope here).

### Task 8.5 — full-suite green gate + coverage sweep

Run `go test ./...` and `make test-race`. Confirm: no existing test changed behavior; the same
`session.jsonl` bytes (projection ≡ direct); segment archival untouched; every Stage-A `Kind`
is emitted by the driven tests (turn/model/tool/spawn/summarize/loop-trip/error). If green,
Stage A is landable on its own.

---

## Stage B — streaming token deltas (fenced; fold in only if clean)

> Gate: attempt Stage B ONLY after Stage A is green and committed. If Stage B threatens Stage
> A's green state or balloons in scope, STOP, drop the Stage B commits, and defer it to its own
> change — note the deferral in the PR body + results. Prefer a clean Stage-A-only PR.

### Task 9 — streaming path on `Completer`

**Files:** `internal/agent/agent.go` (interface), fakes.

- Add a streaming sibling without breaking `Complete`: an **optional** capability interface
  `type StreamingCompleter interface { CompleteStream(ctx, req, func(delta string)) (model.CompletionResp, error) }`
  — the loop type-asserts `a.model.(StreamingCompleter)`; if absent, falls back to `Complete`
  (so any Completer that does not implement streaming is unaffected — additive).

### Task 10 — implement streaming in both adapters

**Files:** `internal/model/adapter.go` (`Adapter`), `cmd/fuse/cli_adapter.go` (`CLIAdapter`).

- LiteLLM HTTP `Adapter`: implement `CompleteStream` using SSE/`stream:true` if the gateway
  supports it; the callback receives each token delta. If the gateway double used in tests
  buffers, guard behind a capability probe and keep `Complete` as the non-streaming path.
- `CLIAdapter`: stream the Claude subprocess's incremental output through the callback.
- Both must produce a final `CompletionResp` identical to today's `Complete` (deltas are
  additive observability, not a semantic change).

### Task 11 — emit `model.delta` between call.start and call.end

**Files:** `internal/agent/loop.go`.

- When `a.model` is a `StreamingCompleter`, call `CompleteStream` with a callback that
  `a.emit(KindModelDelta, turn, ModelDeltaPayload{Text: delta})` per chunk, between the
  `model.call.start` and `model.call.end` emissions. Non-streaming path is unchanged.

**Tests:** a fake `StreamingCompleter` emits N deltas; assert N `model.delta` events land in
order between start and end; assert a non-streaming Completer emits zero deltas and the loop is
unchanged; run the full suite green under `-race`.

---

## Out of scope (do NOT touch)

- `SpawnFunc` signature (stays `(string, error)` — change 0044).
- `cmd/fuse/mcp_server.go` (the tool-call MCP server — change 3's surface).
- Deleting the direct `Logger.Write` call site (trivial follow-up once equivalence proven).
- The segment store / ADR-0020 (left untouched; overlap accepted).
- Reattach/resume wiring; networked EventStore backend; gzip/rotation of events.jsonl.

## Verification (maps to spec §Verification)

1. **Additive-ness** — full suite green with Stage A; no existing behavior changed; identical
   `session.jsonl` bytes; segment archival + spawn results unchanged.
2. **Round-trip** — Append→Replay(0) equal in Seq order; late Subscribe + Replay(from)
   reconstructs history to the cursor.
3. **Non-blocking** — a stalled subscriber never wedges Append/the loop (ADR-0016 guard), under
   `-race`.
4. **Coverage** — every emitted `Kind` produced by a driven run (tool call + spawn + summarize +
   induced error). (Stage B adds `model.delta` coverage.)
