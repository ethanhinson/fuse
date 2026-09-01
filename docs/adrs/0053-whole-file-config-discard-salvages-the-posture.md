---
id: 53
slug: whole-file-config-discard-salvages-the-posture
title: A whole-file config discard is fail-safe only while every dimension defaults to the safe side — adding a permissive default obliges the loader to salvage the posture
status: Accepted
date: 2026-09-01
supersedes: []
reverses: []
relates_to: [44]
change: 64
---

## Context

Change #63's sandbox config loader carries a deliberate whole-file rule: a YAML syntax or type error, or an unknown `handler:` value, discards the **entire** config and returns `DefaultConfig()` with a loud warning. The reasoning was sound and remains so for what it covered — the other fields (`image`, `env_passthrough`) *widen* what a command can see, and they were written by the same hand that typed something the loader could not understand, so partially honouring such a file is worse than ignoring it.

That rule was uniformly fail-**safe** for a reason that was never stated as a precondition: the default on every dimension the config then carried (`contained`) was the safe side. Discarding could only ever move the result toward safety.

Change 0064 added `egress.mode`, whose default (`allow-all`) is the **unsafe** side. That silently inverted the rule's safety property. An operator who wrote `mode: enforce` and then mistyped an unrelated key elsewhere in the same file got the whole file discarded and therefore **unrestricted egress** — while believing they had enforced it. A whole-branch review caught this before merge; the spec is explicit that egress must "fail toward deny-all (floor on, empty allowlist), never toward the internet."

## Decision

On the two whole-file discard paths, **salvage the posture and nothing else**.

1. The loader attempts a permissive second decode of just `egress.mode`. When the file is so broken that no decoder can reach the value, it falls back to a line-oriented, comment-stripped text scan for `mode:`.
2. If the recovered posture is recognizably enforcing — **including an unrecognised mode spelling**, which a parseable file already resolves to enforcement — the result is `Egress{Mode: EgressEnforce}` with an **empty allowlist**: the floor on, deny-all.
3. The salvage is deliberately biased so that **every way it can misjudge resolves toward enforcement**.
4. The salvage has **no path to an allow entry**. An entry recovered from a file you could not parse would be an authorization decision derived from untrusted bytes. Deny-all is the correct outcome for a broken file that asked for enforcement.
5. Every other dimension's whole-file behavior is unchanged from #63.

## Consequences

**The transferable rule a future contributor needs:** a whole-file discard is a fail-safe strategy *only while every dimension it discards has a safe default*. Adding a dimension whose default is permissive silently converts that strategy into a fail-**open** one, and it does so without touching the discard code — nothing in the loader changes, nothing fails a test, and the inversion is invisible at the diff of the change that caused it. Therefore **each new config dimension must be audited against the discard paths, not only against its own parse path.** The audit question is not "does my field parse correctly?" but "if the whole file is thrown away, is my field's default still the side I want to land on?"

**A recovered enforcement posture is maximally restrictive, by design.** An operator whose broken file asked for enforcement gets deny-all, not their intended allowlist — commands that would have been permitted are refused until the file is fixed. That is the intended cost: the loud warning still fires, and a refused command is a diagnosable failure whereas silent unrestricted egress is not.

**Residual, deliberately left open.** `WarnNoRoot` and `WarnUnreadable` — an absent file, or one that cannot be read at all (e.g. `chmod 000`) — still resolve to `allow-all`. There are no bytes on those paths from which any posture could be derived, so closing that gap would mean either inventing a posture the operator never wrote or making the loader fallible. Neither was acceptable for this change; a future decision may revisit it if the deployment model makes an unreadable config a likelier failure than a missing one.
