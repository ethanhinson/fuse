---
id: 59
slug: 'steering-and-the-orchestrator-labelled-optionally-interrupti'
title: 'Steering and the orchestrator - labelled, optionally interrupting messages to a running loop, delivered on every binding and across replicas'
status: 'Accepted'
date: '2026-10-30'
supersedes: [21, 22]
reverses: []
relates_to: [6, 16, 21, 22, 33, 47]
change:
---

## Context

This ADR is recorded after the fact. The steering and orchestrator work shipped on main in commits 005fc45 through 17d84a2 (merged 2026-10-12, documented in c5b175a) without a docket change or an ADR. It records the decision as the code implements it.

Before it, human-to-agent messaging was the per-node human-message bus built in b65dc6e (2026-08-09) from two never-accepted proposals: ADR-0021 (the human as a participant node, three phases) and ADR-0022 (one HumanMsg substrate: a per-node queue self-pulled at the turn boundary, a completion hook that bubbles undelivered messages to the parent, @handles, /btw asides, an async advisory LLM router). Both proposals held ADR-0016's run-to-completion line: a message reaches a node only where it yields (an in-flight ask_user or the top of a turn), and interrupting a running node was explicitly out of scope. That left three gaps: a long model generation or a slow command held a message back for the whole step; messaging existed only inside the interactive shell, not on the one-shot CLI, the stdio binding, the networked binding or the SDKs; and nothing could watch an agent and correct it on a person's behalf.

## Decision

1. A steer is a message to a running loop's root agent carrying a source label and an optional interrupt flag. It rides the existing HumanBus (internal/agent/humanmsg.go): HumanBus.Steer enqueues it, and the root agent's turn-boundary drain delivers it, as before.
2. Interrupt is real mid-step preemption. For the span of each turn the agent loop registers an interrupt hook (HumanBus.SetInterrupt); an interrupting steer fires it, cancelling the in-flight model call or tool batch with a distinct cause (errSteered). Tool calls that had not started return a well-formed skipped result and a call that was running has its result marked, so provider call/result pairing stays valid. A message that arrives while the final answer is being produced gets one more turn instead of being dropped.
3. Sources are human (the default) and orchestrator. Orchestrator guidance reaches the agent under its own label, framed as spoken for the person and newer than earlier instructions. On the wire the label is informational, not a credential (a client can already put any text in a message); values other than the two are rejected at the edge.
4. The orchestrator is an ordinary persistent chat loop whose only tools are loop_status, loop_event and steer_agent (internal/orchestrator), reaching its target through a transport-free Target interface. It has no shell or file access, and its own tools are not permission-gated because the person chatting with it is the approver; the steered agent's tool calls stay gated. orchestrator.model picks its model and is honored only from the trusted home config (ADR-0006).
5. Steering is a Runtime capability, not a new seam: Steerer (Steer with source and interrupt) and Shutdowner are optional capabilities a binding type-asserts for (runtime.SendOrSteer falls back to Send, so an interrupt degrades to a next-boundary message on a runtime that cannot interrupt). StartLoop with Orchestrate starts an orchestrator loop; the target must be in the same tenant and is authorized by the same ownership rule as Send.
6. Every way to run fuse exposes it: fuse.loop.v1 SendRequest gains interrupt and source and StartLoopRequest gains orchestrate_loop_id (Connect, ADR-0033); the stdio binding's loop.send/loop.start gain the same fields; the Go and TS SDKs gain Interrupt/SendWith and Orchestrate; the shell gains /interrupt and /orchestrator; a one-shot run started with --control serves the loop service on a private unix socket (mode 0600, no bearer token), and fuse steer / fuse orchestrate drive any loop over a socket or a server URL.
7. Any replica can deliver to any loop. A Send or Steer that lands on a replica that does not own the loop is written to the loop's durable stream as a steer.request event before the call is accepted; the owning replica reads its own stream and queues it like a local message. The user.input event that delivers it lists the request, so a restarted owner redelivers exactly what is still pending. An expired lease marks the loop not live everywhere, and a graceful drain ends the replica's loops (Shutdowner) so they hand off at once.

Relation to ADR-0021 and ADR-0022, which this supersedes: kept from them, as built in b65dc6e, are the HumanMsg value type, the per-node queue self-pulled at the turn boundary, the completion-hook bubbling, @handles, /btw asides, the async advisory router in the shell, and ask_user's Chat answer. Changed: their run-to-completion line, which forbade preemption, is replaced by interrupt steers; delivery reaches every binding and replica instead of the interactive shell only. Not adopted: storing the queue in blackboard keys (humanq/<nodeID>) or delivering through the blackboard inbox mailbox - the bus is a typed in-memory layer with its own mutex and sequence, and the durable cross-replica path is the loop's event stream, not the blackboard. Steering and the orchestrator target a loop's root agent; addressing a subagent directly remains the shell's @handle path.

## Consequences

Enables: a person, or an orchestrator model acting for them, can redirect an agent mid-step instead of waiting out a long generation or command; the same steering works in the shell, the one-shot CLI, the stdio and networked bindings, both SDKs, and across replicas behind a load balancer; the orchestrator is a normal loop, so it is observed, authorized, persisted and resumed like one.
Costs: an interrupt discards the in-flight model output and marks interrupted or skipped tool results as errors, which the agent must recover from on its next turn; cross-replica delivery adds a polling relay (a 2s bound when no wake-up arrives) and steer.request events in every loop stream; the control socket's only protection is file mode 0600; the source label is not authenticated, so it is presentation, not policy.
Given up: ADR-0021/0022's guarantee that a running step is never preempted.

## Alternatives considered

- Keep run-to-completion and only shorten turns: does not help a single long generation or command, which is the case that needed steering.
- A separate steering service or bidirectional stream per loop: a new seam and transport; the Runtime capability plus Send/StartLoop fields reuse the Connect wire (unary + server-streaming only, ADR-0033) and every existing binding.
- An orchestrator with general tools (shell, files): rejected so the orchestrator can only watch and talk, never act around the steered agent's permission gate.
- Routing cross-replica messages by replica address or sticky sessions: rejected for writing the request into the loop's durable stream, which needs no replica-to-replica addressing and survives a crash on either side.
- Blackboard-backed queues (ADR-0022's humanq keys): the blackboard is per session and per process; the typed bus plus the durable event stream covers both in-process ordering and cross-process delivery.
