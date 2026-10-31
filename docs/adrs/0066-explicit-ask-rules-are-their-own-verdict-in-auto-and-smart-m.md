---
id: 66
slug: 'explicit-ask-rules-are-their-own-verdict-in-auto-and-smart-m'
title: 'Explicit ask rules are their own verdict in auto and smart mode: prompt a human, deny at the rules layer when only a policy stand-in is bound'
status: 'Superseded by ADR-0070'
date: '2026-10-31'
supersedes: [64]
reverses: []
relates_to: [5, 49, 61]
change: 92
---

## Context

ADR-0064 made an explicit ask rule match its own verdict in auto mode, but smart mode still let an always_prompt match fall through to its implicit allow step when no human approver was bound, so a hosted smart-mode loop with an always_prompt rule ran the command without anyone being asked. ADR-0064 also listed --approve-all as a policy stand-in binding, which is wrong: --approve-all runs mode off, where no rule is evaluated and nothing is asked. Change 0092 closes the smart-mode gap and corrects the binding list.

## Decision

- An explicit ask rule match (auto.ask or always_prompt in auto mode; always_prompt in smart mode) is its own verdict in both modes. It never falls through to the safelist, heuristics, the classifier, or the smart-mode allow step.
- With a human approver bound, the match prompts that human.
- With only a policy stand-in bound, the call is denied at the rules layer with reason "matched ask rule <pattern>; no approver bound" and decided_by: policy.
- Policy stand-in bindings are: the stdio loop server, the networked loop server, the headless mcp-server (no HITL socket), and the piped-stdin one-shot deny fallback. --approve-all is NOT a policy stand-in binding: it runs mode off, where no rule is evaluated and nothing is asked. ADR-0064 wrongly listed it; this ADR corrects that.
- When no rule matches, behavior is unchanged in both modes.

## Consequences

- Hosted smart-mode loops with an always_prompt rule that previously ran the matching command now deny it. Operators who want it to run must remove or narrow the rule, or bind a human approver.
- The pre-approval ask event in smart mode now carries layer rules and the ask-rule reason, matching the auto-mode event shape, so one rule produces one event shape regardless of mode.
- The binding list is now accurate; nothing about --approve-all changes in behavior.

## Alternatives considered

- Routing the smart-mode ask to the policy stand-in: this is effectively what is chosen, by removing the implicit allow that previously short-circuited it.
- A smart-only reason string: rejected, because it is one rule and should have one event shape across modes.
- Denying at LayerSmartConfig: rejected, because always_prompt is an operator rule and its auto-mode twin already reports the rules layer.
