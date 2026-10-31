---
id: 65
slug: 'permission-patterns-match-by-consumer-generous-for-protectiv'
title: 'Permission patterns match by consumer: generous for protective rules, strict for permissive rules'
status: 'Accepted'
date: '2026-10-31'
supersedes: []
reverses: []
relates_to: [5, 49]
change: 88
---

## Context

Permission patterns such as `bash:git *` or `bash:rm build/*` were matched with strict path.Match semantics everywhere. Under path.Match, `*` never spans `/`, so a protective rule (deny, ask, always_prompt) written as `bash:curl *` silently failed to match `curl https://host/path`, and protective rules failed open on any argument containing a slash or on compound commands. The same pattern language is also used by permissive consumers (auto_approve and smart-mode allow), where generous matching would widen what is admitted without a prompt. Change 0088 needed one rule that fixes the protective fail-open without widening allow.

## Decision

Permission pattern matching is split by consumer.

- Protective consumers (deny, ask, always_prompt) match generously via matchProtective in internal/permissions/cmdglob.go: `*` spans `/`, and a pattern is tested against each per-segment subject `bash:<argv0> <args>`, against the whole command, and against the `bash:<argv0>` head. Any hit applies the protective rule.
- Permissive consumers (auto_approve and smart-mode allow) keep strict path.Match semantics, so `bash:git *` still does not match `git clone https://evil/x` and `bash:rm build/*` does not admit `rm build/../x`.
- Smart mode keeps its legacy first-token subjects and additionally evaluates the segment subject; any allow widening this introduces stays guarded by the isDangerous floor.

## Consequences

Protective rules now fire on arguments containing slashes and on any segment of a compound command, closing the fail-open. Allow rules keep their egress and path scoping, so no command becomes auto-approved that was not before. The cost is two matching semantics for one pattern syntax: the same pattern can match a command for deny but not for allow, which rule authors and future code must keep in mind. Erring generous on the protective side means some deny or ask rules may fire more often than their authors intended; that is the safe direction.

## Alternatives considered

Make all patterns generous: rejected because it widens auto_approve and smart allow, letting `bash:git *` admit `git clone` to arbitrary URLs and breaking egress scoping. Make all patterns strict: rejected because protective rules keep failing open whenever an argument contains a slash, which is the defect being fixed.
