---
id: 68
slug: 'catastrophic-floor-is-a-rules-layer-decision-in-smart-mode-a'
title: 'Catastrophic floor is a rules-layer decision in smart mode and headless bindings; protective patterns match the command as written'
status: 'Accepted'
date: '2026-10-31'
supersedes: []
reverses: []
relates_to: [5, 49, 65]
change: 93
---

## Context

The catastrophic-command floor and protective rules behaved differently across permission modes and bindings. In smart mode an allow rule (legacy first-token or per-segment) could approve a catastrophic command before the floor was consulted, and with only a policy stand-in bound (no human) a stand-in could approve it. Protective patterns (deny, ask, always_prompt) matched only the parsed, wrapper-peeled segment, so rules naming a wrapper such as timeout, nice, env, or bash -c never fired in auto or smart mode. The headless mcp-server gate did not bind workspace root and write roots the way every other binding does, so its decisions diverged from the rest of the runtime. This builds on ADR-0005, ADR-0049, and ADR-0065.

## Decision

1. In smart mode the catastrophic-command floor is evaluated at the rules layer before any allow rule, legacy first-token or per-segment. With a human bound, a catastrophic command prompts with reason "catastrophic command: <cmd>". With only a policy stand-in bound, it is denied at the rules layer with reason "catastrophic command denied by rules floor: <cmd>". Scope: commands the parser accepts. Commands the parser refuses (sudo, npx, path-qualified argv0) keep their prior behavior, and prompt-all mode with a stand-in is unchanged. An rm or dd segment containing an unresolved shell expansion no longer matches a legacy first-token allow.

2. Protective patterns (deny, ask, always_prompt) match the raw, pre-peel command text as written in addition to the parsed segment, so rules naming a wrapper (timeout, nice, env, bash -c) fire in auto and smart mode. Permissive patterns (allow) keep matching only the parsed strict form.

3. The headless mcp-server gate binds workspace root and write roots like every other binding.

## Consequences

A stand-in can no longer approve a catastrophic command in smart mode, and allow rules cannot pre-empt the floor. Wrapper-naming protective rules now fire in auto and smart mode, which can surface more prompts or denials for commands users previously ran unchallenged. Allow rules are not widened, so the raw-text match only ever tightens. Known residual: wrapper rules still do not fire in auto mode for forms the parser refuses. Side effect of binding roots on the mcp-server gate: in-workspace and scratch writes are now auto-approved there in auto mode, a documented loosening toward parity with the other bindings.

## Alternatives considered

A separate floor applied only in auto mode was rejected because in smart mode a policy stand-in still approved catastrophic commands. Widening allow globs to cover wrapper forms was rejected because it loosens permissive matching instead of tightening protective matching, and would let wrapped forms bypass intent.
