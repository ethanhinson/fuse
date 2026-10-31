---
id: 64
slug: 'explicit-ask-rules-are-their-own-verdict-prompt-a-human-deny'
title: 'Explicit ask rules are their own verdict: prompt a human, deny when no human approver is bound'
status: 'Superseded by ADR-0066'
date: '2026-10-31'
supersedes: []
reverses: []
relates_to: [5, 49, 61]
change: 87
---

## Context

Auto-mode permissions let operators declare ask rules (permissions.auto.ask and always_prompt matches) for commands that must reach a human. Previously an ask match fed into the same pipeline as an unmatched command, so the safelist, heuristics, or classifier could still resolve it, and where no human approver was bound (hosted loop servers, --approve-all, headless mcp-server) the policy stand-in could approve it. An explicit ask therefore could silently become an implicit allow, contradicting the operator's stated intent.

## Decision

An explicit auto-mode ask rule match produces its own verdict, VerdictAskExplicit, which short-circuits the safelist, heuristic, and classifier layers. When a human approver is bound, the command prompts that human. When only a policy stand-in is bound (hosted loop servers, --approve-all, headless mcp-server), the command is denied at the rules layer with reason "matched ask rule <pattern>; no approver bound". An explicit ask is never converted into an implicit allow. The "no rule matched" path is unchanged and continues through the existing safelist, heuristic, and classifier evaluation.

## Consequences

Operator ask rules now mean what they say in every runtime: a human sees the command, or it does not run. Denials carry a reason naming the matched pattern, so headless and hosted operators can see why. Compatibility: hosted loops and other headless configurations that declare ask rules will now deny those commands where they previously may have been allowed; operators who want them to run unattended must move the pattern to an allow rule. Per-segment evaluation (ADR-0005) and allowlist admission on deterministic allow paths (ADR-0049) are unaffected for commands that do not match an ask rule.

## Alternatives considered

Letting the classifier or safelist override an ask match was rejected because it makes the operator's explicit rule advisory. Auto-approving ask matches under a policy stand-in was rejected because it is exactly the implicit allow this decision removes. Failing the whole loop when an ask rule matches headlessly was rejected as too disruptive; a per-command deny with a clear reason lets the agent adapt.
