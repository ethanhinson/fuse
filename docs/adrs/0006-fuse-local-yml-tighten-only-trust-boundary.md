---
id: 6
slug: fuse-local-yml-tighten-only-trust-boundary
title: .fuse.local.yml cannot loosen permission policy (repo-plantable config trust boundary)
status: Accepted
date: 2026-08-05
supersedes: []
reverses: []
relates_to: [5]
change: 17
---

## Context

fuse merges a `.fuse.local.yml` from the current working directory into the effective
config (`internal/config/loader.go`). Because that file lives in the working tree, it is
**repo-plantable**: a cloned or malicious repository can ship a `.fuse.local.yml` that, if
honored wholesale, would silently loosen the permission gate — e.g. flip `permissions.mode`
to `off`, enable `session_allow`, add broad `auto_approve` allow patterns, or set
`permissions.auto.*` classifier/rule knobs — turning the auto-mode gate off for anyone who
runs fuse inside that checkout.

This mirrors the trust-config threat every mature harness designs against: Claude Code never
reads permission trust config from project files (only user/managed settings); Grok Build
pins managed policy with compiled-in Ed25519 pubkeys. A tool meant to run autonomously
inside untrusted checkouts must treat repo-plantable config as an attacker-controlled input.

## Decision

`.fuse.local.yml` is honored **only for permission-tightening**; permission-loosening keys
are ignored with a warning. Specifically, the loosening keys — `mode` (when it would relax
the gate), `auto_approve`, `session_allow`, and the entire `auto.*` subtree
(`classifier_model`, and the deny/ask rule lists that could widen approval) — are stripped
from any CWD-merged `.fuse.local.yml` with a logged warning at load time. Tightening keys
(e.g. `always_prompt`, `disabled`) remain honored from the same file, because a repo asking
for **more** friction is never a privilege escalation.

This is enforced in the loader's `.fuse.local.yml` merge path, **not** in the gate, so the
loosening values never reach the effective config at all.

## Consequences

A repo-planted `.fuse.local.yml` can never weaken the permission gate of the human who
clones it — the highest-value trust-boundary hardening for a tool meant to run autonomously
inside untrusted checkouts. To loosen policy, a user must set those keys in a location the
user controls (user-level / home config, or the primary committed config they authored),
never a repo-plantable file.

Costs: a genuine per-repo desire to loosen (a trusted monorepo wanting a broader
`auto_approve` set) cannot express that through `.fuse.local.yml` and must use a user-scoped
config — a deliberate friction. The asymmetry (tighten-yes / loosen-no) must be maintained as
new permission keys are added: each new key must be classified loosening-or-tightening, or it
defaults to loosening (ignored) fail-safe.
