---
id: 49
slug: auth-multi-tenancy
title: Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [48]
related: [45, 46, 47, 48, 50, 51, 53]
discovered_from: [45, 47]
adrs: []
spec: docs/superpowers/specs/2026-08-11-auth-multi-tenancy-design.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-auth-multi-tenancy-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-auth-multi-tenancy-design.md) |
<!-- docket:artifacts:end -->

## Why

The networked binding (change 48) is single-tenant — every caller shares one trust domain, an
explicit non-goal carried forward from 0045. A **deployed** service that many users' apps target
needs authentication and multi-tenancy: each caller may only start, drive, observe, and replay the
loops it owns, and loops are isolated per tenant. Needs brainstorming: the identity/auth model,
loop_id ownership, and per-tenant isolation boundaries over the networked seam.

## What changes

An auth + multi-tenancy layer over the networked binding (change 48). Full design in the linked
spec; at proposal altitude:

- **Bearer-token identity behind a pluggable `Verifier` seam.** A token on the WS handshake and HTTP
  replay request resolves to a `Principal{tenant, subject}`. A static/configured token verifier
  ships as the default; the seam is shaped so OIDC/JWT/mTLS verifiers slot in later without a re-cut
  (the same two-implementation discipline as the store seam). The verifier lives at the **binding
  edge** — `internal/runtime` gains no auth import and keeps the policy-free seam (ADR-0030).
- **Token is authoritative for tenant.** Tenant is derived solely from the verified principal; the
  wire `tenant` field is never trusted (validated must-match-or-rejected). `loop.start` derives
  `(tenant, owner)` from the token so all three verbs converge on one token-derived `(tenant, loop)`
  key — the fix for finding 1.
- **Ownership binding + per-request authorization.** `loop.start` records `(tenant, owner=subject)`
  in the durable registry; every `send`/`observe`/`replay` authorizes against it. Cross-tenant →
  `loop not found`; cross-owner (same tenant) → a distinguishable **`forbidden`** error (finding 2).
  Ownership is read from the durable registry (source of truth), not the in-memory cache.
- **Owner liveness lease (heartbeat + TTL).** `LoopRecord` gains a lease; the owning instance
  heartbeats to renew it; a cold instance treats `Live && lease-expired` as abandoned and may
  reap/re-own — the same lease shape docket uses for claim reclaim (finding 3). Added to both the
  fsstore and Postgres backends behind the existing conformance suite.
- **Reconnect is first-class and re-authorized.** Change 48's client-driven stateless reconnect
  (track last `Seq`, re-observe `from=<lastSeq>`, ride subscribe-before-replay + dedup-at-watermark)
  is preserved and re-authorized on every handshake. **Re-own on reconnect:** when the reconnecting
  caller is the recorded owner and the prior owner's lease has expired (redeploy/crash), the new
  instance re-owns, renews the lease, and resumes serving — so "attach to your running loop from your
  phone after the server redeployed" works end-to-end.

### Findings folded in from the 0047 dogfood (discovered_from: 47)

The design closes three gaps a real `fuse loop-server` dogfood surfaced, all verified present in the
merged code: (1) `loop.start` carries no tenant so the binding is single-tenant; (2) loop ownership
is recorded but unenforced on the wire; (3) a hard-killed server leaves `Live: true` forever with no
lease to distinguish live from abandoned. See the spec for how each is closed.

## Out of scope

- The WS/HTTP transport itself — change 48 (done).
- Client SDK ergonomics / versioned external wire envelope — change 50.
- Observability emission (OTEL, `/metrics`) — change 51; this change only carries the already-threaded
  context and `(tenant, loop, node)` triple.
- Rich verifiers (OIDC/JWT signature checking, mTLS, token issuance/rotation/revocation) — the seam
  is shaped for them; only the static bearer impl ships here.
- TLS termination / deployment topology / cross-instance load-balancing — operational concerns.
- A nested per-org→per-user isolation hierarchy — the boundary here is `(tenant, owner)`.
- Resuming a *completed* one-shot loop's execution on re-own (re-own serves replay for a finished
  loop; resuming a parked persistent loop rides change 53's persistence primitive).
