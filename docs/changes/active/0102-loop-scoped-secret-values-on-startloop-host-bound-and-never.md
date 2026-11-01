---
id: 102
slug: 'loop-scoped-secret-values-on-startloop-host-bound-and-never'
title: 'Loop-scoped secret values on StartLoop, host-bound and never recorded'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: [100, 101]
stacked_on:
related: []
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-01-loop-scoped-secret-values-on-startloop-host-bound-and-never-design.md'
plan: 'docs/superpowers/plans/2026-11-01-loop-scoped-secret-values-on-startloop-host-bound-and-never.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/loop-scoped-secret-values-on-startloop-host-bound-and-never'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-01-loop-scoped-secret-values-on-startloop-host-bound-and-never-design.md](../../superpowers/specs/2026-11-01-loop-scoped-secret-values-on-startloop-host-bound-and-never-design.md) |
| Plan | [2026-11-01-loop-scoped-secret-values-on-startloop-host-bound-and-never.md](../../superpowers/plans/2026-11-01-loop-scoped-secret-values-on-startloop-host-bound-and-never.md) |
<!-- docket:artifacts:end -->

## Why

a networked server resolves `secret_ref` names only from the operator's `loop_server.secrets` (env or file on the server host, granted per tenant). A client that brings its own MCP servers has no way to bring its own credentials for them: the operator would have to put every client's token into the server process environment and restart, which is both operationally wrong and discloses each client's secret to the operator's config. The spec deliberately carries only secret names (it is recorded durably by `loop.harness`), so the values need a transport-level, memory-only channel.

## What changes

- `StartLoopRequest.secrets` (`repeated LoopSecret {name, value, hosts}`), documented as loop-scoped, in-memory only, never recorded, redacted from logs and errors; at most 32 entries, value at most 16 KiB.
- `harnessresolve.Resolver` consults loop-scoped secrets after operator secrets: an operator entry for the tenant wins; otherwise a loop-scoped entry resolves the name only when the destination URL's hostname is in that entry's `hosts` (a loop-scoped secret with empty `hosts` is a violation at `secrets[i].hosts`). Names must match `SecretRefPattern`.
- The Connect handler requires scope `secrets:inline` (unrestricted principals included) when `secrets` is non-empty, else `CodePermissionDenied`; the runtime seam receives the values through `LoopConfig` and they are dropped when the loop ends.
- `loop.harness` keeps recording the spec with names only; a redaction test asserts no loop-scoped value ever appears in the event store, logs or a `ValidationError` text.
- `sdk/fuse` `StartLoop` options gain `WithSecrets(...)`.
- ADR recording: loop-scoped secrets are transport-level, host-bound, memory-only; operator entries take precedence.

## Out of scope

a pluggable remote `SecretResolver` (file as later); secrets for `Send`; rotation during a loop; encrypting values at rest (there is no rest).

## Reconcile log

### 2026-11-01

Unchanged against main 728079e. Deps 0100 (secrets:inline constant) and 0101 are merged. Refinements: loop-scoped values travel from the Connect edge to the resolver through a small leaf package (runtime seam stays auth-free); ConfigSecrets-first precedence with loop-scoped fallback; GetCapabilities fields 7/8 untouched.
