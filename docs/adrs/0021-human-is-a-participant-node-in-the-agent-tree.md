---
id: 21
slug: human-is-a-participant-node-in-the-agent-tree
title: The human is a first-class participant in the agent tree — questions route from the asking node and replies route back to it, in three phases
status: Proposed
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [7, 16, 23]
change: 0050
---

## Context

The `ask_user` tool (a question overlay modeled on Claude Code's AskUserQuestion)
shipped with a **"Chat about this"** row that only *dismisses* the question:
selecting it sends `Answer{Cancelled: true}`, identical to Esc. The label promises
a conversation the harness cannot have. That mislabel exposed a deeper gap.

Fuse is a multi-agent harness: a turn fans out into a spawn tree of subagents
(ADR-0016), coordinating through a shared blackboard (change 0023), observable
through the `/agents` overlay. But the **human can only address the root turn**.
When a subagent three levels down needs a decision, the harness has no way to
route that question *from* that specific node, and no way for the human to inject a
steer *into* that node's context. The human is outside the tree looking in, not a
participant in it.

A good harness treats the human as another node: any live agent can ask the human
a question, the human knows *which* agent asked, and the human can open a
side-channel to *any* node — not just the root. This ADR records the decision to
build toward that, and — importantly — the architectural constraint that shapes
how far each phase can reach.

The load-bearing constraint is ADR-0016's runtime shape: **a subagent runs to
completion synchronously.** `spawn_agent.Execute` "blocks until it completes"
(`internal/tools/spawn_agent.go`); a node's message history is assembled at spawn
and returned at finish; nodes and events are append-only. There is **no inbound
channel into a running node**. Any design that claims to "message a running
subagent mid-run" must either (a) ride a seam the node voluntarily blocks on, or
(b) change the run-to-completion contract. This ADR does not change that contract;
it works within it.

Two facts about the current `ask_user` wiring frame the near-term work:

- `AskFunc` is `func(ctx, Question) (Answer, error)` — it **carries no node
  identity**. A child's question reaches the TUI anonymous.
- `ask_user` is registered **once on the root registry**
  (`cmd/fuse/shell.go`), inherited by children via clone — unlike the blackboard,
  which is wired **per-child, node-bound** (`wireChildBlackboard(... childNode)`),
  and unlike approvals, which are **label-prefixed per child**
  (`permissions.PrefixApproval(opts.Label, ...)`).

## Decision

Model the human as a **first-class participant in the agent tree**, delivered in
three phases of increasing reach. Each phase is independently shippable and useful;
later phases depend on earlier ones.

### Phase 1 — Reply-to-asker (local, no tree changes)

"Chat about this" stops being a dismiss. Selecting it opens a **prose input** whose
text returns to the **asking agent** as the `ask_user` result — a new
`Answer.Chat string` field distinct from `FreeText` (an off-menu *pick*) and
`Cancelled` (a *decline*). Semantics: "none of your options fit — here is the
context you were missing; re-ask or adapt." The tool result carries the chat text
so the model can act on it in its next turn.

This needs no tree or runtime change: it rides the existing `ask_user` →
`AskFunc` → TUI overlay → `RespCh` loop already in place. It also makes the label
honest immediately.

### Phase 2 — Per-node question routing (identity, no runtime change)

Thread the **asking node's identity** through the question path so the human sees
*which* agent is asking and the reply routes back to exactly that node:

- Extend `AskFunc` to `func(ctx, Question) (Answer, error)` carrying node identity,
  mirroring how approvals carry provenance — either by a `PrefixAsk(label, ...)`
  wrapper (cheapest, matches `PrefixApproval`) or by adding an `AskerID`/`AskerLabel`
  to the wire request.
- Wire `ask_user` **per-child, node-bound** in the spawn factory
  (`makeSpawnFunc` in `cmd/fuse/shell.go`), exactly where `wireChildBlackboard`
  and `PrefixApproval` already bind child identity — instead of inheriting one
  anonymous root-registered tool.
- The overlay shows the asker (`☐ [coder] Header`) and, when multiple nodes ask
  concurrently, the FIFO queue already distinguishes them.

Still no runtime change: a node asking a question already blocks on `RespCh`
(the `AskFunc` call is synchronous within that node's turn), so routing the reply
back to that same blocked call is identity plumbing, not a new inbound channel.

### Phase 3 — Human-initiated chat to any node (new inbound seam)

The human opens a side-channel to a **live node the human selects** in `/agents`
(which already has `selected`, `nodeByID`, `inDetail`, and per-node key handlers):
press a key on a node to send it a message that lands in **its** context.

This is the phase that meets ADR-0016 head-on. Because a running node has no
inbound channel, Phase 3 delivers the message through a seam the node **chooses to
block on**, rather than interrupting it:

- **Blackboard mailbox (preferred first cut).** The blackboard already defines an
  inbox convention — `InboxKey(target, seq)` → `inbox/<target>/<seq>` and
  `InboxPattern(self)` → `inbox/<self>/*` (`internal/agent/blackboard.go`). The
  human `Put`s to `InboxKey(<nodeID>, seq)`; a node that opted in wakes on it
  (`Blackboard.Put` already wakes parked waiters). Zero runtime change; the node
  must voluntarily listen (`blackboard_wait` on its inbox, or poll it between
  turns). Human writes get **`writerLabel = "human"`** provenance so the board and
  `/agents` render the human as a node. Reusing the existing inbox convention means
  Phase 3 adds a UI affordance and a `writerLabel`, not a new transport.
- A node that never waits never hears the message — an accepted limitation of the
  run-to-completion contract, surfaced in the UI ("queued; delivered when the agent
  next checks its inbox") rather than hidden.

Interrupting a *non-listening* running node — true preemption — is explicitly **out
of scope** for this ADR: it would change ADR-0016's synchronous run-to-completion
contract and belongs in its own decision.

## Consequences

- **The label stops lying immediately** (Phase 1): "Chat about this" does what it
  says. Until Phase 1 lands, the row is renamed to a plain dismiss ("Skip — you
  decide") so the UI is never misleading in the interim.
- **Identity is plumbed where identity already flows** (Phase 2): the design adds
  no new provenance mechanism — it extends `AskFunc` to match the approval path and
  wires `ask_user` per-node like the blackboard, closing an inconsistency (ask_user
  is today the only human-facing tool that loses node identity).
- **The human becomes a node on the board** (Phase 3): human↔agent messages reuse
  the blackboard's existing mailbox (`Put` wakes `Wait`), with `writerLabel="human"`
  provenance, so the `/agents` overlay and blackboard tab render the human as a
  participant with no new transport.
- **Run-to-completion is preserved, and its cost is made honest.** The harness does
  not gain mid-run preemption here. A message to a node that isn't listening is
  *queued and surfaced as such*, never silently dropped and never presented as
  delivered. True interrupt-a-running-node is deferred to a separate ADR that would
  revisit ADR-0016.
- **Blast radius is staged.** Phase 1 touches only `ask_user`/`ask.go`. Phase 2
  touches the `AskFunc` signature and its wiring sites (root + child). Phase 3 adds
  a `/agents` affordance and a mailbox convention. No phase rewrites the runtime.

## Alternatives considered

- **Rename only, build nothing.** Honest but abandons a capability the substrate
  (tree, blackboard, HITL relay) already all but affords. Rejected as the end
  state; adopted only as the Phase-1 interim.
- **A dedicated per-node inbound message channel + `Agent.Run` refactor.** True
  mid-run injection requires `Agent.Run` (`internal/agent/loop.go`) to poll an
  external message channel alongside model/tool completions, and each node to know
  its own ID for routing — today `Run` takes history only at entry and mutates a
  local slice to completion (`ctx.Done()` is the only outside lever). Cleanest for
  real chat, but it breaks ADR-0016's append-only, run-to-completion model and
  invites the depth-2 deadlock class the slot-yield discipline exists to prevent.
  Deferred to its own ADR rather than smuggled in here.
- **Route all human→node chat through the HITL Unix socket relay** (`internal/hitl`).
  The relay is request/response per approval, not a durable mailbox; reusing the
  blackboard (already a session-scoped, provenance-stamped, wait-able store) is a
  smaller, more honest fit for Phase 3.
