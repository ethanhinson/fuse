---
id: 70
slug: 'approval-provenance-is-per-gate-explicit-ask-rules-supersede'
title: 'Approval provenance is per gate; explicit ask rules (supersedes ADR-0066)'
status: 'Accepted'
date: '2026-10-31'
supersedes: [66]
reverses: []
relates_to: [5, 49, 61]
change: 94
---

## Context

ADR-0066 made an explicit ask rule match its own verdict in auto and smart mode, and decided that only a human approver may answer it; with a policy stand-in bound the call is denied at the rules layer. It enumerated the policy stand-in bindings and relied on a process-wide mark (markPolicyApproval) to tell the gate which kind of approver it had. That mark was per process, not per gate: bindings that forgot to call it (the research probe, the orchestrator's deny stand-in) were reported as human, and a process holding both a human and a policy approver could not label both correctly. Change 0094 moves provenance onto the approver itself.

## Decision

- An explicit ask rule match (auto.ask or always_prompt in auto mode; always_prompt in smart mode) is its own verdict in both modes. It never falls through to the safelist, heuristics, the classifier, or the smart-mode allow step. (Unchanged from ADR-0066.)
- With a human approver bound, the match prompts that human. (Unchanged.)
- With a policy stand-in bound, the call is denied at the rules layer with reason `matched ask rule <pattern>; no approver bound` and decided_by: policy. (Unchanged.)
- When no rule matches, behavior is unchanged in both modes. (Unchanged.)
- Provenance is a property of the approver, not the process. permissions.Approver carries {Answer, DecidedBy} and is built with permissions.Human or permissions.Policy; a gate's provenance is its approver's. A process may hold human and policy approvers at once, and each gate reports its own. New panics on an unlabelled approver, and there is no process-wide mark.
- Policy stand-ins: permissions.AlwaysApprove wherever it is bound (both loop servers, the headless mcp-server, the research probe, and any child built with it), the piped-stdin deny fallback, --approve-all's approver (moot, since that run is mode off and nothing is asked), and the orchestrator's deny stand-in. Human approvers: the TUI channel, the one-shot terminal prompt, and the mcp-server's HITL socket client. This replaces ADR-0066's policy stand-in bindings list.

## Consequences

- Research-probe explicit-ask matches and smart-mode catastrophic commands now deny with decided_by: policy instead of being misreported as human-decided.
- Orchestrator stand-in denials now record decided_by: policy.
- A new binding cannot silently inherit the wrong provenance: an approver without a label panics at construction.
- Known limitation: a CLI-model gate with a policy approver still relays prompts to an mcp-server subprocess, which labels its socket client human.

## Alternatives considered

- Recognising AlwaysApprove by function identity: rejected, because wrappers such as PrefixApproval erase that identity.
- Keeping the process-wide global and adding the missing markPolicyApproval calls: rejected, because the next binding forgets, and one global cannot label a process that holds both kinds of approver.
- A per-binding option on buildGate: rejected, because it still leaves two values (the approver and the option) that can disagree.
