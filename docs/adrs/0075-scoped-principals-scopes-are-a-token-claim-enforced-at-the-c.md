---
id: 75
slug: 'scoped-principals-scopes-are-a-token-claim-enforced-at-the-c'
title: 'Scoped principals: scopes are a token claim enforced at the Connect edge'
status: 'Accepted'
date: '2026-11-01'
supersedes: []
reverses: []
relates_to: []
change: 100
---

## Context

Every verified principal could previously call every RPC. Credentials need to be narrowable (for example start-only or observe-only) without changing tenant isolation, and issuers will mint new scope names over time. Principal is used as a map key, so its fields must be comparable.

## Decision

Scopes are a claim on the verified principal (issuer JWT `scope` space-separated string; static token `scopes:` list), enforced per RPC at the Connect edge as the first check, before tenant resolution. Vocabulary: loop:start, loop:send, loop:observe, loop:admin, capabilities:read. Absent scope means unrestricted (backward compatible). Present-but-empty, null, whitespace-only or non-string is refused; a JSON-array `scope` is refused. Unknown scope names are ignored but still make the token restricted (forward compatible with newer issuers; typos fail closed). `loop:admin` is explicit-grant only; it satisfies Send/Observe and lifts only the owner comparison; tenant isolation still holds and it does not grant loop:start. `Principal.Scopes` is a canonical space-joined string type (not a slice) because Principal is a map key. Harness digest pin: optional `harness_digest` claim; StartLoop refuses unless it exactly equals the computed digest of the submitted spec, before the runtime or registry. It is not consulted on Send/Observe/resume (loop records carry no digest); pair `harness_digest` with `scope: loop:start` for a start-only credential. Enforcing the pin on Send/Observe is a possible follow-up. `azp` (client id), `jti` and issuer name are carried on the principal for audit only. Every loop records a `loop.principal` event right after `loop.harness` (never on resume); a failed append fails the launch. Tool-identity broker mints emit `tool.identity.minted` audit events.

## Consequences

Credentials can be narrowed per RPC with no change for existing unscoped tokens, and issuer rollouts of new scopes do not break verification. Start-only credentials are possible via scope plus digest pin. Costs: the pin is not enforced on Send/Observe/resume until loop records carry a digest; the sandbox egress proxy still has no audit sink (follow-up related to change 57); a restricted token with only unknown scopes can do nothing.

## Alternatives considered

Refusing unknown scopes: rejected, it breaks issuer rollouts. Scopes implying admin: rejected, admin is explicit-grant only. Enforcing the digest pin on every RPC: deferred, it needs a digest on loop records.
