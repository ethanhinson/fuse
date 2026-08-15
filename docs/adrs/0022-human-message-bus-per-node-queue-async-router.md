---
id: 22
slug: human-message-bus-per-node-queue-async-router
title: Human messages ride one substrate — a per-node blackboard queue, self-pulled at turn boundaries, with an async advisory router and completion-hook bubbling
status: Proposed
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [7, 16, 21, 23]
change: 0051
---

## Context

ADR-0021 established the human as a participant node and sketched three phases.
This ADR specifies the **single substrate** all four concrete human-messaging
features share, and was designed through an adversarial two-model debate on the
harness itself (`kimi` proposed, `glm` critiqued and counter-designed; both read
the live code). The debate surfaced correctness holes a single pass missed; this
ADR adopts the hardened design.

The four features, one underpinning:

1. **Respond-to-agent** — reply in prose to the agent currently asking; returns as
   that agent's `ask_user` result.
2. **@agent-direct** — address a named live node (`@coder`); handles auto-derived
   from label+index, optionally human-renamed, tab-completable.
3. **/btw aside** — a read-only status question answered by the **harness** from
   tree/blackboard state; no model call, no agent interruption; cannot action.
4. **Queued + editable messages** — messages typed while an agent is busy queue and
   deliver at the next **turn boundary** (never mid-run, per ADR-0016); the human
   can edit/reorder/delete queued items; an **LLM router** intelligently classifies
   target+mode.

The binding constraint is ADR-0016: a subagent runs to completion synchronously;
there is **no inbound channel into a running node**. Delivery may therefore only
happen where the node *voluntarily yields*: inside an in-flight `ask_user`, or at
the top of a turn in `Agent.Run` (`loop.go:360`, before `Complete` at 443).

## Decision

**One value type — `HumanMsg` — stored in a per-node blackboard queue, self-pulled
by the node at its turn boundary, with an async advisory router and a completion
hook that bubbles undelivered messages up the tree.**

### Core types (`internal/agent/humanmsg.go`)

```go
type MsgMode int
const (
    ModeRespond   MsgMode = iota // synchronous answer to an in-flight ask_user
    ModeDirect                   // @handle; turn-boundary injection into one node
    ModeBroadcast                // @all; injected into every live node
    ModeAside                    // /btw; harness-answered from state, NEVER delivered
    ModeQueued                   // bare text; default-routed, router may reclassify
)

type MsgStatus int
const (
    StatusPending MsgStatus = iota // queued, not yet delivered
    StatusRouted                   // router classified it (TUI shows target)
    StatusDelivered                // drained at a turn boundary
    StatusStranded                 // target finished; bubbled to parent
    StatusUndeliverable            // bubbled to root, root also finished
)

type HumanMsg struct {
    ID       string    // uuid
    ToNodeID string    // resolved at enqueue; "" for Aside/Broadcast
    Mode     MsgMode
    Text     string
    Seq      uint64    // GLOBAL monotonic sequence — strict ordering, never TS
    TS       time.Time // display only, NOT ordering
    Status   MsgStatus
    Handle   string    // display metadata only, looked up fresh at render
}
```

### Queue storage — one key per node

| key | value | mutability |
|---|---|---|
| `humanq/<nodeID>` | `[]HumanMsg` ordered by `Seq` | mutable — one `Put` rewrites the whole slice |
| `humanlog/<seq>` | `HumanMsg`, `Status >= Delivered` | append-only transcript |

A single key per node (not per-message keys) makes edit/reorder/delete a
Get-modify-Put on one slice, makes the node's drain one Get + one Delete, and
eliminates the TS-collision and stale-enumeration races a per-message-key scheme
invites. `WriterLabel = "human"` provenance renders the human as a node on the
board. This is a staging buffer; ADR-0016's append-only rule governs nodes/events,
not this pending-edit queue. The `humanlog/` record is the append-only side.

### Delivery — three run-to-completion-safe paths

1. **Respond (synchronous).** The node is already blocked in `ask_user` →
   `AskFunc` → TUI overlay. The prose is `Answer.Chat` (ADR-0021 Phase 1),
   returning as the tool result. Ephemeral: logged to `humanlog/`, never enters
   `humanq/`. Multiple simultaneous asks are disambiguated by `selectedAsk()` — the
   `/agents`-selected node if it has a pending ask, else the most recent asker.

2. **Direct / Queued / Broadcast (turn boundary).** `HumanInjector.Poll` runs at
   the top of the turn loop: the node's own goroutine reads its own
   `humanq/<nodeID>` (a **self-pull**, never a cross-goroutine push), drains it in
   `Seq` order, and injects **one batched** `user` message (segments joined by
   `---`) — not N consecutive user messages, which confuse models. Broadcast
   enqueues an identical copy into every live node's queue; each drains
   independently (no cross-node ordering guarantee, none needed).

3. **Completion hook (`AgentTree.OnNodeComplete`).** When a node finishes with a
   non-empty `humanq/`, its messages are **bubbled to the parent** (marked
   `StatusStranded`, re-homed to `ParentOf(nodeID)`), surfaced in the TUI as
   "was for @coder (finished) → redirected". At root (always live — the human's
   partner), any residue is marked `StatusUndeliverable` and surfaced, never
   silently dropped. This closes the stranded-message hole: a node whose final turn
   emits no tool call exits without another turn boundary, so `Poll` alone would
   lose a late message.

### The LLM router — async, advisory, non-blocking

The router is **not** in the submit path. The submit handler is deterministic and
instant in every rung:

1. `/btw …` → aside (harness-answered).
2. `@all …` → broadcast enqueue.
3. `@handle …` → resolve handle → direct enqueue (fallback: selected node).
4. pending ask + free text → respond (`selectedAsk()`).
5. bare text → **immediate** default enqueue (selected/root, `ModeQueued`) **plus**
   a fire-and-forget `Router.ClassifyAsync`.

`ClassifyAsync` runs a cheap structured-output model (e.g. `deepseek-flash`,
3s timeout) over `{text, live nodes [handle,label,status,depth,lastTool], selected}`
→ `{mode, handle}`. If it picks a specific node it moves the message; on
error/timeout/unknown-handle the default routing stands. The human's input latency
is never coupled to the router's — a slow/absent router degrades to manual routing
via the queue editor, not a frozen prompt.

### /btw — structured intent+target parse, not a keyword bag

`ParseAside` deterministically extracts an `@handle` target and classifies intent
(`AsideStatus | AsideLastTool | AsideWrites | AsideCount | AsideTree | AsideUnknown`).
`AnswerAside` reads only race-safe state (`SnapshotAll`, `CopyEvents`,
`ActiveCounts`, `bb.Snapshot`) and renders a transcript line — never delivered to a
node. `AsideUnknown` returns an explicit "I can answer: status, last-tool, writes,
count, tree" fallback, so an off-template question is guided, not silently dropped.

### @handles — auto + rename

`HandleRegistry` on `AgentTree`: auto-derive at `addNode` (`sanitizeHandle(label)`,
collision → `-2/-3`), optional `/rename @old @new` (re-points `byHandle`, NodeID
stable). Resolve handle→NodeID **at enqueue** (rename between enqueue and delivery
cannot misroute). `HumanMsg.Handle` is display-only, looked up fresh from `byNode`
at render so renames reflect immediately.

## Consequences

- **One substrate, four features.** Respond, @direct, @all, /btw, and queued all
  reduce to a `HumanMsg` + the per-node queue + the three delivery paths. New
  message kinds are new `MsgMode` values, not new machinery.
- **Run-to-completion preserved.** No mid-run injection; nodes self-pull at yield
  points. The cost (turn-boundary latency) is explicit and mitigated by the queue
  editor and /btw giving live visibility during a run.
- **No silent loss.** The completion hook + `MsgStatus` make every message's fate
  visible (delivered/stranded/undeliverable). This was the debate's fatal finding.
- **Human input never blocks on an LLM.** The router is advisory; submit is
  deterministic and instant.
- **Staged blast radius.** New file `humanmsg.go` (types + injector + registry +
  router + aside); TUI submit-path routing, queue editor, /btw and @-completion;
  one new tree hook (`OnNodeComplete`) and handle registration at `addNode`. The
  agent loop gains exactly one call (`HumanInjector.Poll`) at the turn top.

## Alternatives considered (from the debate)

- **Per-message queue keys (kimi).** Rejected: TS-collision ordering, stale-Keys
  enumeration races, N+1 blackboard ops per drain. Single-key slice is simpler and
  race-free.
- **Synchronous router in the submit path (kimi).** Rejected: couples human input
  latency/availability to the router. Made async/advisory.
- **TS ordering (kimi).** Rejected for a global monotonic `Seq`.
- **`TargetRef` "exactly one non-zero" union (kimi).** Rejected as unenforceable in
  Go; flattened to a resolved `ToNodeID` + display-only `Handle`.
- **Keyword-bag /btw (kimi).** Rejected for a structured intent+target parser with
  an explicit unknown-fallback.
- **Ignoring stranded messages (kimi).** Rejected — the completion-hook bubble-up is
  load-bearing, not optional.
- **True mid-run preemption.** Out of scope (would revisit ADR-0016); deferred.
```
