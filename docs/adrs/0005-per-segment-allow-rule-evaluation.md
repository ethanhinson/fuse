---
id: 5
slug: per-segment-allow-rule-evaluation
title: Per-segment allow-rule evaluation — a deliberate deviation from the Grok Build reference
status: Accepted
date: 2026-08-05
supersedes: []
reverses: []
relates_to: []
change: 17
---

## Context

Change 0017 models fuse's auto-mode permission pipeline on Grok Build
(xai-org/grok-build), the fullest open-source implementation of the layered permission
stack. Grok Build splits compound commands on `&&`/`||`/`;`/`|`/newlines and evaluates
DENY and ASK rules per-segment (and against the whole string), but — by its own
documentation — evaluates ALLOW rules against the whole command string only. Grok's docs
explicitly admit this leaves a hole: `Bash(git *)` auto-approves `git status && rm -rf /`,
because the whole-string allow match succeeds even though a dangerous second segment rides
along.

This is the same bypass class behind Gemini CLI's real-world CVE (Tracebit: an allowlisted
`grep` prefix with a `; exfil` appended) and Cursor's denylist-wrapping bypass. It is the
single highest-value correctness hole in the surveyed permission-gate designs: a
whole-string allow match that silently green-lights a piggy-backed dangerous segment.

## Decision

Fuse deliberately deviates from the Grok reference — it evaluates ALLOW rules PER-SEGMENT
too (matching Claude Code's behavior). A compound command is auto-approved only if EVERY
parsed segment independently matches an allow rule (against a real shell AST from
`mvdan.cc/sh/v3`, not a raw-string glob). One non-matching or denied segment rejects the
whole command.

This is implemented via the deterministic shell parser in
`internal/permissions/shellparse.go` (`splitSegments` → per-segment `Segment` list) feeding
the per-segment rule engine in `internal/permissions/rules.go`. Deny still wins globally
regardless of rule order (CWE-1188 default-deny hardening).

## Consequences

- Closes the single highest-value correctness hole in the surveyed permission-gate designs
  (the whole-string allow bypass) — the Gemini/Cursor bypass class cannot slip a
  piggy-backed segment past an allow prefix.
- Cost: a legitimate compound command where one segment is not independently allow-listed
  now prompts or denies rather than auto-approving. This is a conservative posture that
  costs a human prompt, never a silent bypass.
- Fuse's allow rules cannot be a drop-in for a Grok / `.claude` settings file that relied on
  whole-string allow semantics for compound commands — fuse is strictly stricter.
- Ties fuse to real AST parsing (the `mvdan.cc/sh` dependency) rather than cheap glob
  matching.
