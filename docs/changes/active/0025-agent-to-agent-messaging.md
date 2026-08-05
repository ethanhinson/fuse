---
id: 25
slug: agent-to-agent-messaging
title: Agent-to-agent messaging — note passing for debate/refine patterns
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [12, 23]
related: [23, 26]
discovered_from: [23]
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
<!-- docket:artifacts:end -->

## Why

The blackboard (change 0023) enables indirect agent-to-agent communication via shared state, but it lacks a direct messaging primitive: an agent cannot send a targeted message to a specific peer, cannot request a peer's attention, and cannot engage in back-and-forth dialogue. Patterns like debate (two agents critique each other's positions), review (one agent asks another for feedback), and refinement (agent A asks agent B to improve its output) require directed, conversational messaging. Adding a lightweight `agent_message` tool that sends a note to a named peer — and a message inbox that the target can check — enables these patterns without the overhead of spawning and joining sub-sub-agents.

## What changes

- **`agent_message(target, message, context?)` tool**: a new built-in tool alongside `spawn_agent` and `blackboard_*`. Sends a text message to a named agent (by label or node ID). The `context` field is an optional JSON string providing shared context (e.g. the code snippet under discussion).
- **Message inbox on `AgentNode`**: each node gains a thread-safe message queue (buffered channel, capacity 16). Incoming messages are appended to the node's event log as `MessageReceivedEvent{from, text, context}`.
- **`agent_inbox()` tool**: a new built-in tool that returns pending messages for the calling agent (non-destructive read) and marks them as read. Parameters: `clear` (boolean, default false — also removes them).
- **Message delivery semantics**: best-effort, in-order per sender. If the target agent has exited (`StatusDone`/`StatusError`/`StatusCancelled`), the message is returned as an error to the sender ("agent X is no longer running"). If the inbox is full (16 pending), the oldest message is dropped (head-drop, not tail-drop — freshest messages survive).
- **Agent tree visualization**: the tree overlay shows a message count badge on nodes with unread messages (e.g. `agent-c (3)`).
- **Transcription**: all messages (sent and received) appear in the agent's event log, visible in the tree detail pane drill-down.

## Out of scope

- Guaranteed delivery or acknowledgements — best-effort only.
- Multi-cast or broadcast — send to one agent at a time.
- Structured message types beyond plain text + optional JSON context.
- Cross-session messaging — messages are ephemeral (in-memory, same as the blackboard).

## Research notes (input for the brainstorm)

The debate/refine pattern works as follows: parent spawns agent A (propose solution) and agent B (critique). A calls `agent_message("agent-b", "Here's my proposal...")`. B checks its inbox, formulates a critique, sends `agent_message("agent-a", "This approach has issue X...")`. A checks its inbox, refines, sends back. This loop continues until one agent signals convergence or a turn limit. The parent can monitor the conversation via the event logs and intervene if it stalls. The key design constraint is that messaging must not create deadlock cycles: if every agent is waiting for a message and none can proceed, the system stalls. The inbox capacity of 16 with head-drop prevents unbounded memory growth but could lose messages under heavy load — the blackboard (change 0023) remains the reliable path for important data. The research skill could use this for multi-perspective debate: one agent argues for a position, another argues against, a third synthesizes the debate into a balanced report.
