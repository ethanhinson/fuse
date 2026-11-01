---
id: 100
slug: 'scoped-principals-rpc-scopes-harness-digest-pin-and-principa'
title: 'Scoped principals: RPC scopes, harness digest pin and principal record on every loop'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: [99]
stacked_on:
related: []
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-01-scoped-principals-rpc-scopes-harness-digest-pin-and-principa-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-01-scoped-principals-rpc-scopes-harness-digest-pin-and-principa-design.md](../../superpowers/specs/2026-11-01-scoped-principals-rpc-scopes-harness-digest-pin-and-principa-design.md) |
<!-- docket:artifacts:end -->

## Why

a `Principal` is `{Tenant, Subject, ObservabilityOperator}`, so every bearer a hosting operator issues is all-or-nothing within its tenant: a credential meant only to observe a loop can start one; one meant to start one specific harness can start any spec. `StartLoop` computes `harness_digest` and returns it but never checks it against anything, so an issuer cannot say "this token may run exactly this spec". No event in a loop's durable stream names the credential that started it, and the tool-identity broker runs with a nil audit sink in production (`cmd/fuse/tool_identity.go:249`), so an operator cannot answer "who started this loop and under which client".

## What changes

- `loopauth.Principal` gains `Scopes []string`, `HarnessDigest string`, `ClientID string` (JWT `azp`), `Issuer string`, `TokenID string` (JWT `jti`). `IssuerVerifier` reads `scope` (space-separated string per RFC 8693 §4.2), `harness_digest`, `azp`, `jti`. Static tokens get `scopes` (optional list) in `AuthTokenConfig`.
- Scope vocabulary, as constants in `loopauth`: `loop:start`, `loop:send`, `loop:observe`, `loop:admin`, `capabilities:read`, `secrets:inline` (defined here, used by F4). Empty `Scopes` means unrestricted (today's behaviour). The handler enforces: `StartLoop` needs `loop:start`; `Send` needs `loop:send`; `Observe` needs `loop:observe`; `GetCapabilities` needs `capabilities:read`; an unknown scope string is ignored. Refusal is `CodePermissionDenied` with a message naming the missing scope.
- `loop:admin` lets `Send`/`Observe`/orchestrate act on any loop in the principal's tenant regardless of `Owner`; without it the owner rule is unchanged.
- Digest pin: when `Principal.HarnessDigest` is set, `StartLoop` refuses (`CodePermissionDenied`) a request whose `harness.Digest(m.Harness)` differs or whose `harness` is unset; the check runs before the runtime is touched.
- A new event `loop.principal` (`event.KindLoopPrincipal`, payload `{subject, client_id, issuer, scopes, token_id}`) is appended right after `loop.harness` at `StartLoop` when a principal is present; redaction rules unchanged (no token material).
- The tool-identity broker gets an audit sink in `loop-serve-net` that appends a `tool.identity.minted` event (`{subject, audience, scope, loop_id}`) to the loop's stream.
- `sdk/fuse` `RemoteCredentials` gains `TokenProvider func(ctx) (string, error)`; when set it is called per request instead of `Token`. (Small, keeps SDK clients able to use short-lived issuer tokens; no refresh loop.)
- ADR recording: scopes are a claim, enforced at the Connect edge, unrestricted when absent; digest pin; principal and mint audit events.

## Out of scope

`jti` replay cache or revocation; scope-based narrowing of the HarnessSpec (model or MCP allowlists are F3); per-loop ACLs beyond owner/admin; SDK token refresh loops; TS SDK parity (file as later).
